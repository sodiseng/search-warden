package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Contract test with the official Go SDK client (ARCHITECTURE.md §18): a real
// MCP client connects, sees exactly the official tools and calls them.

type bearer struct{ base http.RoundTripper }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return b.base.RoundTrip(r)
}

func connect(t *testing.T, f *fixture, path string) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	client := sdk.NewClient(&sdk.Implementation{Name: "contract-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: f.gw.URL + path,
		HTTPClient: &http.Client{Transport: bearer{http.DefaultTransport}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", path, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestOfficialGoClient(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()

	for _, c := range []struct {
		path, provider string
		want           []string
	}{
		{"/tavily/mcp", "tavily", []string{"tavily_search", "tavily_extract", "tavily_crawl", "tavily_map", "tavily_research"}},
		{"/exa/mcp", "exa", []string{"web_search_exa", "web_fetch_exa", "agent_run"}},
	} {
		cs := connect(t, f, c.path)
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("%s tools/list: %v", c.path, err)
		}
		var names []string
		official := map[string]json.RawMessage{}
		for _, raw := range snapshotTools(t, c.provider) {
			official[raw.Get("name").String()] = json.RawMessage(raw.Get("inputSchema").Raw)
		}
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
			got, _ := json.Marshal(tool.InputSchema)
			var a, b any
			_ = json.Unmarshal(got, &a)
			_ = json.Unmarshal(official[tool.Name], &b)
			if ga, _ := json.Marshal(a); string(ga) != string(mustJSON(b)) {
				t.Errorf("%s: the client sees a different input schema than the official server's", tool.Name)
			}
			if tool.Description == "" || tool.Annotations == nil {
				t.Errorf("%s: description or annotations lost", tool.Name)
			}
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s tools = %v", c.path, names)
		}
	}

	// Calls, through the merged endpoint.
	f.up.always("POST /search", 200, `{"query":"go","results":[{"url":"https://go.dev","title":"Go","content":"x","score":0.9}],"usage":{"credits":1},"costDollars":{"total":0.007}}`)
	f.up.always("POST /contents", 200, `{"results":[{"title":"Go","url":"https://go.dev","text":"hello"}],"costDollars":{"total":0.001}}`)
	cs := connect(t, f, "/mcp")
	r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "tavily_search", Arguments: map[string]any{"query": "go"}})
	if err != nil || r.IsError || len(r.Content) != 1 {
		t.Fatalf("tavily_search: %+v %v", r, err)
	}
	if txt := r.Content[0].(*sdk.TextContent).Text; !strings.Contains(txt, `"url":"https://go.dev"`) || strings.Contains(txt, "usage") {
		t.Fatalf("tavily_search text = %s", txt)
	}
	if r.StructuredContent == nil {
		t.Fatal("tavily_search: no structuredContent (the official outputSchema promises it)")
	}
	r, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "web_fetch_exa", Arguments: map[string]any{"urls": []string{"https://go.dev"}}})
	if err != nil || r.IsError || r.Content[0].(*sdk.TextContent).Text != "# Go\nURL: https://go.dev\n\nhello" {
		t.Fatalf("web_fetch_exa: %+v %v", r, err)
	}
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unknown tool accepted")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
