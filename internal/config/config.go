// Package config loads and compiles rum-config.json, the single source of truth
// shared by beacon.js (client) and the collector (server). It resolves a host to
// a service identifier and a path to a path_group, and validates client-supplied
// labels so that a malicious client cannot explode VictoriaMetrics cardinality.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
)

// PathRule is a single glob pattern -> group label mapping.
type PathRule struct {
	Pattern string `json:"pattern"`
	Group   string `json:"group"`
}

type rawConfig struct {
	Services         map[string]string     `json:"services"`
	DefaultService   string                `json:"default_service"`
	DefaultPathGroup string                `json:"default_path_group"`
	PathGroups       map[string][]PathRule `json:"path_groups"`
}

type compiledRule struct {
	re    *regexp.Regexp
	group string
}

// Config is an immutable, compiled view of rum-config.json. Build it with Compile
// and never mutate it; the Loader swaps whole Config values atomically on reload.
type Config struct {
	raw              []byte
	services         map[string]string
	defaultService   string
	defaultPathGroup string

	pathGroups map[string][]compiledRule // service id (or "*") -> ordered rules

	validServices map[string]struct{}            // allowed service label values
	groupSets     map[string]map[string]struct{} // service id (or "*") -> allowed group label values
}

// Raw returns the original JSON bytes, served verbatim at /rum-config.json so the
// client reads exactly what the server validates against.
func (c *Config) Raw() []byte { return c.raw }

// DefaultService is the fallback service label (e.g. "unknown").
func (c *Config) DefaultService() string { return c.defaultService }

// DefaultPathGroup is the fallback path_group label (e.g. "other").
func (c *Config) DefaultPathGroup() string { return c.defaultPathGroup }

// globToRegExp mirrors beacon.js: * matches a single segment, ** matches across
// segments, and the whole path is anchored. Keeping the two implementations in
// lockstep is what lets client and server agree on path_group.
func globToRegExp(glob string) (*regexp.Regexp, error) {
	const marker = "\x01DBLSTAR\x01"
	g := strings.ReplaceAll(glob, "**", marker)
	g = strings.ReplaceAll(g, "*", marker+"SINGLE")
	quoted := regexp.QuoteMeta(g) // escapes everything; markers are inert ASCII
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta(marker+"SINGLE"), "[^/]*")
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta(marker), ".*")
	return regexp.Compile("^" + quoted + "$")
}

// Compile parses and validates rum-config.json into an immutable Config.
func Compile(raw []byte) (*Config, error) {
	var rc rawConfig
	if err := json.Unmarshal(raw, &rc); err != nil {
		return nil, fmt.Errorf("parse rum-config.json: %w", err)
	}

	c := &Config{
		raw:              raw,
		services:         rc.Services,
		defaultService:   rc.DefaultService,
		defaultPathGroup: rc.DefaultPathGroup,
		pathGroups:       make(map[string][]compiledRule, len(rc.PathGroups)),
		validServices:    make(map[string]struct{}),
		groupSets:        make(map[string]map[string]struct{}, len(rc.PathGroups)),
	}
	if c.services == nil {
		c.services = map[string]string{}
	}

	for svc, rules := range rc.PathGroups {
		compiled := make([]compiledRule, 0, len(rules))
		set := make(map[string]struct{}, len(rules)+1)
		for _, r := range rules {
			re, err := globToRegExp(r.Pattern)
			if err != nil {
				return nil, fmt.Errorf("service %q pattern %q: %w", svc, r.Pattern, err)
			}
			compiled = append(compiled, compiledRule{re: re, group: r.Group})
			set[r.Group] = struct{}{}
		}
		if rc.DefaultPathGroup != "" {
			set[rc.DefaultPathGroup] = struct{}{}
		}
		c.pathGroups[svc] = compiled
		c.groupSets[svc] = set
	}

	for _, id := range rc.Services {
		c.validServices[id] = struct{}{}
	}
	if rc.DefaultService != "" {
		c.validServices[rc.DefaultService] = struct{}{}
	}

	return c, nil
}

// ResolveService maps a hostname to its service identifier, falling back to
// default_service. This is the same logic beacon.js runs client-side.
func (c *Config) ResolveService(host string) string {
	if id, ok := c.services[host]; ok {
		return id
	}
	if c.defaultService != "" {
		return c.defaultService
	}
	return "unknown"
}

// ResolvePathGroup maps a path to its group for the given service, evaluating
// rules in declaration order and taking the first match.
func (c *Config) ResolvePathGroup(service, path string) string {
	rules, ok := c.pathGroups[service]
	if !ok {
		rules = c.pathGroups["*"]
	}
	for _, r := range rules {
		if r.re.MatchString(path) {
			return r.group
		}
	}
	if c.defaultPathGroup != "" {
		return c.defaultPathGroup
	}
	return "other"
}

// ValidateService returns service if it is an allowed label value, else
// default_service. The bool reports whether the value was rejected (for the
// label_rejected meta-metric).
func (c *Config) ValidateService(service string) (string, bool) {
	if _, ok := c.validServices[service]; ok {
		return service, false
	}
	return c.fallbackService(), true
}

// ValidatePathGroup returns group if it is an allowed value for service, else
// default_path_group. The bool reports whether the value was rejected.
func (c *Config) ValidatePathGroup(service, group string) (string, bool) {
	set, ok := c.groupSets[service]
	if !ok {
		set = c.groupSets["*"]
	}
	if set != nil {
		if _, ok := set[group]; ok {
			return group, false
		}
	}
	return c.fallbackPathGroup(), true
}

func (c *Config) fallbackService() string {
	if c.defaultService != "" {
		return c.defaultService
	}
	return "unknown"
}

func (c *Config) fallbackPathGroup() string {
	if c.defaultPathGroup != "" {
		return c.defaultPathGroup
	}
	return "other"
}

// AllowedOrigins returns https origins for every configured service host, used to
// build the /collect CORS allowlist.
func (c *Config) AllowedOrigins() []string {
	origins := make([]string, 0, len(c.services))
	for host := range c.services {
		origins = append(origins, "https://"+host)
	}
	return origins
}

// Loader holds the current Config and supports atomic hot-reload from disk.
type Loader struct {
	path string
	cur  atomic.Pointer[Config]
}

// NewLoader reads and compiles the config at path immediately; an invalid file is
// a startup error.
func NewLoader(path string) (*Loader, error) {
	l := &Loader{path: path}
	if err := l.Reload(); err != nil {
		return nil, err
	}
	return l, nil
}

// Reload re-reads and re-compiles the config file, swapping it in atomically. A
// parse error leaves the previous config in place.
func (l *Loader) Reload() error {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", l.path, err)
	}
	c, err := Compile(raw)
	if err != nil {
		return err
	}
	l.cur.Store(c)
	return nil
}

// Get returns the current compiled config.
func (l *Loader) Get() *Config { return l.cur.Load() }
