package referrer

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"mastodon.social", "mastodon.social", true},
		{"  WWW.Google.CO.JP. ", "www.google.co.jp", true},
		{"xn--wgv71a119e.jp", "xn--wgv71a119e.jp", true}, // punycode is how browsers hand it out
		{"", "", false},
		{"   ", "", false},
		{"Ünïcode.example", "", false},
		{"evil.example/../x", "", false},
		{"a.example:8080", "", false}, // the beacon sends hostname, never host:port
		{strings.Repeat("a", 254), "", false},
	}
	for _, tc := range cases {
		got, ok := Normalize(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Normalize(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestClassify(t *testing.T) {
	own := func(h string) bool { return h == "hosteka.example" }
	cases := []struct{ host, want string }{
		{"", Direct},
		{"hosteka.example", Internal},
		{"www.google.com", Search},
		{"mastodon.social", Other},
		{"anypost.example", Other}, // another of the operator's services is still other
	}
	for _, tc := range cases {
		if got := Classify(tc.host, own); got != tc.want {
			t.Errorf("Classify(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestIsSearchEngine(t *testing.T) {
	yes := []string{
		// Google, across country domains
		"www.google.com", "google.com", "www.google.co.jp", "google.co.jp",
		"www.google.de", "www.google.com.au", "www.google.com.tw",
		"com.google.android.googlequicksearchbox",
		// Bing, Yahoo
		"www.bing.com", "bing.com", "cn.bing.com",
		"search.yahoo.co.jp", "search.yahoo.com", "us.search.yahoo.com",
		// Japan
		"search.goo.ne.jp", "search.biglobe.ne.jp", "cgi.search.biglobe.ne.jp",
		"search.nifty.com", "websearch.rakuten.co.jp",
		"search.smt.docomo.ne.jp", "search.auone.jp",
		// Others
		"duckduckgo.com", "html.duckduckgo.com", "search.brave.com",
		"www.ecosia.org", "www.startpage.com", "www.qwant.com", "kagi.com",
		"www.baidu.com", "search.naver.com", "yandex.ru", "yandex.com.tr", "ya.ru",
	}
	for _, h := range yes {
		if !IsSearchEngine(h) {
			t.Errorf("IsSearchEngine(%q) = false, want true", h)
		}
	}

	no := []string{
		// Google properties that are not search results
		"news.google.com", "mail.google.com", "docs.google.com", "gemini.google.com",
		"maps.google.com", "googleusercontent.com", "www.youtube.com",
		// Portals around a search box, not the results page
		"www.yahoo.co.jp", "news.yahoo.co.jp", "www.yahoo.com", "www.goo.ne.jp",
		"www.naver.com", "www.msn.com",
		// AI assistants stay out of search on purpose
		"chatgpt.com", "www.perplexity.ai", "perplexity.ai", "copilot.microsoft.com",
		"claude.ai",
		// Look-alikes must not ride the country-domain patterns
		"google.example.com", "www.google.evil.example", "mygoogle.com",
		"notgoogle.co.jp", "google.co.jp.example", "search.yahoo.com.example",
		"yandex.ru.example",
		// Social and the Fediverse
		"t.co", "x.com", "bsky.app", "mastodon.social", "misskey.io",
	}
	for _, h := range no {
		if IsSearchEngine(h) {
			t.Errorf("IsSearchEngine(%q) = true, want false", h)
		}
	}
}
