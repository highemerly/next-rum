package config

import "testing"

const testJSON = `{
  "services": {
    "handon.club":     "handon",
    "mi.hiyoko.club":  "misskey"
  },
  "default_service": "unknown",
  "default_path_group": "other",
  "path_groups": {
    "handon": [
      { "pattern": "/",          "group": "home" },
      { "pattern": "/web",       "group": "web_ui" },
      { "pattern": "/web/**",    "group": "web_ui" },
      { "pattern": "/@*",        "group": "profile" },
      { "pattern": "/@*/**",     "group": "status" },
      { "pattern": "/tags/**",   "group": "tag" },
      { "pattern": "/about**",   "group": "about" }
    ],
    "*": [
      { "pattern": "/", "group": "home" }
    ]
  }
}`

func mustCompile(t *testing.T) *Config {
	t.Helper()
	c, err := Compile([]byte(testJSON))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func TestResolveService(t *testing.T) {
	c := mustCompile(t)
	cases := map[string]string{
		"handon.club":    "handon",
		"mi.hiyoko.club": "misskey",
		"unknown.host":   "unknown", // default_service
	}
	for host, want := range cases {
		if got := c.ResolveService(host); got != want {
			t.Errorf("ResolveService(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestResolvePathGroup(t *testing.T) {
	c := mustCompile(t)
	cases := []struct {
		path string
		want string
	}{
		{"/", "home"},
		{"/@alice", "profile"},            // /@* matches single segment
		{"/@alice/12345", "status"},       // /@* does not match, /@*/** does
		{"/web", "web_ui"},                // exact /web
		{"/web/timelines/home", "web_ui"}, // /web/**
		{"/tags/photo", "tag"},            // /tags/**
		{"/about", "about"},               // /about** (zero trailing)
		{"/about/more", "about"},          // /about** spans segments
		{"/api/v1/timelines", "other"},    // no rule -> default_path_group
	}
	for _, tc := range cases {
		if got := c.ResolvePathGroup("handon", tc.path); got != tc.want {
			t.Errorf("ResolvePathGroup(handon, %q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestResolvePathGroupUnknownServiceUsesStar(t *testing.T) {
	c := mustCompile(t)
	if got := c.ResolvePathGroup("nope", "/"); got != "home" {
		t.Errorf("unknown service fallback to * = %q, want home", got)
	}
	if got := c.ResolvePathGroup("nope", "/whatever"); got != "other" {
		t.Errorf("unknown service no match = %q, want other", got)
	}
}

func TestGlobSegmentBoundaries(t *testing.T) {
	// "*" must not cross a slash; "**" must.
	single, err := globToRegExp("/@*")
	if err != nil {
		t.Fatal(err)
	}
	if single.MatchString("/@a/b") {
		t.Error("/@* should not match across a slash")
	}
	double, err := globToRegExp("/@*/**")
	if err != nil {
		t.Fatal(err)
	}
	if !double.MatchString("/@a/b/c") {
		t.Error("/@*/** should match across slashes")
	}
}

func TestValidateService(t *testing.T) {
	c := mustCompile(t)
	if got, rejected := c.ValidateService("handon"); got != "handon" || rejected {
		t.Errorf("valid service: got %q rejected=%v", got, rejected)
	}
	if got, rejected := c.ValidateService("evil"); got != "unknown" || !rejected {
		t.Errorf("invalid service: got %q rejected=%v, want unknown/true", got, rejected)
	}
	if got, _ := c.ValidateService("unknown"); got != "unknown" {
		t.Errorf("default_service must validate, got %q", got)
	}
}

func TestValidatePathGroup(t *testing.T) {
	c := mustCompile(t)
	if got, rejected := c.ValidatePathGroup("handon", "web_ui"); got != "web_ui" || rejected {
		t.Errorf("valid group: got %q rejected=%v", got, rejected)
	}
	if got, rejected := c.ValidatePathGroup("handon", "evil"); got != "other" || !rejected {
		t.Errorf("invalid group: got %q rejected=%v, want other/true", got, rejected)
	}
	// default_path_group is always allowed for a known service.
	if got, rejected := c.ValidatePathGroup("handon", "other"); got != "other" || rejected {
		t.Errorf("default group: got %q rejected=%v", got, rejected)
	}
}
