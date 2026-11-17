// Package referrer classifies the host of document.referrer into the four
// values the referrer metric may carry: direct, internal, search and other.
//
// The metric label is bounded on purpose: the beacon endpoint is open, so a
// raw host as a label would let anyone mint new series by posting made-up
// hosts. The raw host goes to the Loki line instead, where it costs nothing.
package referrer

import (
	"regexp"
	"strings"
)

// The values of the referrer label. Nothing else is ever returned.
const (
	Direct   = "direct"   // no referrer: typed URL, bookmark, app, or the source withheld it
	Internal = "internal" // the service's own host
	Search   = "search"   // a web search engine (see IsSearchEngine)
	Other    = "other"    // anything else, including the operator's other services
)

// Normalize lowercases host, trims spaces and a trailing dot, and reports
// whether the result is a DNS name (letters, digits, '-', '.'; at most 253
// bytes). Browsers hand out internationalized names in punycode, so a host
// with any other byte did not come from document.referrer.
func Normalize(host string) (string, bool) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || len(h) > 253 {
		return "", false
	}
	for i := 0; i < len(h); i++ {
		b := h[i]
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '.') {
			return "", false
		}
	}
	return h, true
}

// Classify returns the label for a referrer host that Normalize accepted (or
// "" for none). isOwn reports whether a host belongs to the page's service.
func Classify(host string, isOwn func(string) bool) string {
	switch {
	case host == "":
		return Direct
	case isOwn(host):
		return Internal
	case IsSearchEngine(host):
		return Search
	default:
		return Other
	}
}

// searchHosts are the exact hosts a search results page is served from.
//
// Only the results page counts, not the portal around it: www.yahoo.co.jp,
// news.google.com or mail.google.com are not searches. AI assistants
// (chatgpt.com, perplexity.ai, gemini.google.com, copilot.microsoft.com) are
// deliberately left out and count as other; their raw host is in Loki.
var searchHosts = map[string]struct{}{
	// Google's app on Android opens results with an android-app:// referrer,
	// whose hostname is the package name.
	"com.google.android.googlequicksearchbox": {},

	// Microsoft Bing (also behind Edge's address bar and new tab page)
	"bing.com": {}, "www.bing.com": {}, "cn.bing.com": {},

	// Yahoo! JAPAN (results are Google's, but the referrer is Yahoo's)
	"search.yahoo.co.jp": {},

	// Search portals that matter in Japan
	"search.goo.ne.jp":         {}, // goo
	"search.biglobe.ne.jp":     {}, // BIGLOBE
	"cgi.search.biglobe.ne.jp": {},
	"search.nifty.com":         {}, // @nifty
	"websearch.rakuten.co.jp":  {}, // Rakuten
	"search.smt.docomo.ne.jp":  {}, // docomo
	"search.auone.jp":          {}, // au

	// DuckDuckGo
	"duckduckgo.com": {}, "html.duckduckgo.com": {}, "lite.duckduckgo.com": {},

	// Privacy-oriented and independent engines
	"search.brave.com": {},
	"www.ecosia.org":   {}, "ecosia.org": {},
	"www.startpage.com": {}, "startpage.com": {},
	"www.qwant.com": {}, "qwant.com": {}, "lite.qwant.com": {},
	"kagi.com":       {},
	"www.mojeek.com": {},

	// Regional engines
	"www.baidu.com": {}, "m.baidu.com": {}, "baidu.com": {}, // China
	"www.sogou.com": {}, "m.sogou.com": {}, // China
	"www.so.com": {}, "m.so.com": {}, // China (360)
	"search.naver.com": {}, "m.search.naver.com": {}, // Korea
	"search.daum.net": {}, "m.search.daum.net": {}, // Korea
	"ya.ru":            {}, // Russia (Yandex)
	"search.seznam.cz": {}, // Czechia

	// Older portals
	"search.aol.com": {},
	"www.ask.com":    {},
}

// searchPatterns cover engines that serve results from many country domains.
var searchPatterns = []*regexp.Regexp{
	// Google: google.com, google.de, google.co.jp, google.com.au, with or
	// without www. Other google.* subdomains (news, mail, gemini) are not.
	regexp.MustCompile(`^(www\.)?google\.(com|[a-z]{2}|co\.[a-z]{2}|com\.[a-z]{2})$`),
	// Yahoo outside Japan: search.yahoo.com, us.search.yahoo.com, ...
	regexp.MustCompile(`^([a-z]{2}\.)?search\.yahoo\.com$`),
	// Yandex: yandex.ru, yandex.com, yandex.com.tr, ...
	regexp.MustCompile(`^(www\.)?yandex\.(com|[a-z]{2}|com\.[a-z]{2})$`),
}

// IsSearchEngine reports whether host (already normalized) serves web search
// results.
func IsSearchEngine(host string) bool {
	if _, ok := searchHosts[host]; ok {
		return true
	}
	for _, re := range searchPatterns {
		if re.MatchString(host) {
			return true
		}
	}
	return false
}
