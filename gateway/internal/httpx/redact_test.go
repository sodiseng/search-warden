package httpx

import (
	"net/url"
	"testing"
)

func TestRedactQuery(t *testing.T) {
	tests := map[string]string{
		"":                                       "",
		"tavilyApiKey=sw_abc":                    "tavilyApiKey=" + Redacted,
		"exaApiKey=sw_x&tools=web_search_exa":    "exaApiKey=" + Redacted + "&tools=web_search_exa",
		"tools=a,b&key=sw_y&dynamicHighlights=1": "tools=a,b&key=" + Redacted + "&dynamicHighlights=1",
		"TAVILYAPIKEY=x":                         "TAVILYAPIKEY=" + Redacted,
		"api%5Fkey=secret":                       "api%5Fkey=" + Redacted, // escaped key name
		"apiKey=1&apiKey=2":                      "apiKey=" + Redacted + "&apiKey=" + Redacted,
		"q=keyword&monkey=1":                     "q=keyword&monkey=1",
		"token":                                  "token", // no value, nothing to hide
		"access_token=abc&%zz=1":                 "access_token=" + Redacted + "&%zz=1",
	}
	for in, want := range tests {
		if got := RedactQuery(in); got != want {
			t.Errorf("RedactQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	u, _ := url.Parse("https://gw.example.com/tavily/mcp?tavilyApiKey=sw_secret")
	if got := RedactURL(u); got != "/tavily/mcp?tavilyApiKey="+Redacted {
		t.Errorf("RedactURL = %q", got)
	}
}
