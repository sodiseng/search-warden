//go:build live

// Live tests against the real Tavily and Exa APIs. They spend a few credits
// and only run with:  go test -tags live -count=1 -v ./internal/provider/ -run Live
// Keys come from WARDEN_LIVE_TAVILY_KEYS / WARDEN_LIVE_EXA_KEYS or testdata/live.env.
// Nothing secret is logged: keys are masked, response bodies are summarized.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func liveKeys(t *testing.T, name string) []string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		if f, err := os.Open(filepath.Join("..", "..", "testdata", "live.env")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if k, val, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == name {
					v = strings.TrimSpace(val)
				}
			}
			f.Close()
		}
	}
	var keys []string
	for _, k := range strings.Split(v, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		t.Skipf("%s is empty: copy testdata/live.env.example to testdata/live.env and fill it in", name)
	}
	return keys
}

func mask(k string) string {
	if len(k) <= 12 {
		return "****"
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// call sends one request the way the relay will: clean headers, provider auth, rewritten body.
// http.DefaultClient honors HTTPS_PROXY, like the developer's shell.
func call(t *testing.T, p Provider, key, method, path, body string, precise bool) (Operation, int, http.Header, []byte, bool) {
	t.Helper()
	op, ok := p.Match(method, path)
	if !ok {
		t.Fatalf("%s %s not allowed", method, path)
	}
	out, injected, err := p.RewriteRequest(op, []byte(body), precise)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var rd io.Reader
	if method != http.MethodGet {
		rd = bytes.NewReader(out)
	}
	req, _ := http.NewRequestWithContext(ctx, method, p.BaseURL()+op.Path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	p.Authorize(req.Header, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return op, resp.StatusCode, resp.Header, b, injected
}

func topFields(b []byte) string {
	var ks []string
	gjson.ParseBytes(b).ForEach(func(k, _ gjson.Result) bool { ks = append(ks, k.String()); return true })
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func TestLiveTavily(t *testing.T) {
	tv := NewTavily("")
	for _, key := range liveKeys(t, "WARDEN_LIVE_TAVILY_KEYS") {
		t.Run(mask(key), func(t *testing.T) {
			op, status, h, body, injected := call(t, tv, key, "POST", "/search",
				`{"query":"search-warden live test","max_results":1,"search_depth":"basic"}`, true)
			o := tv.Classify(op, status, h, body)
			if o.Class != ClassOK {
				t.Fatalf("search: %d %v %s", status, o.Class, RedactSecret(o.Reason, key))
			}
			cost, ok := tv.ActualCost(op, body)
			if !ok || cost.Credits < 1 {
				t.Fatalf("search: no usage.credits in response (fields: %s)", topFields(body))
			}
			stripped := tv.RewriteResponse(op, body, injected)
			if gjson.GetBytes(stripped, "usage").Exists() {
				t.Fatal("injected usage not stripped")
			}
			t.Logf("search ok: %d credit(s); response fields: %s", cost.Credits, topFields(body))

			op, status, h, body, injected = call(t, tv, key, "POST", "/extract", `{"urls":["https://example.com"]}`, true)
			if o := tv.Classify(op, status, h, body); o.Class != ClassOK {
				t.Fatalf("extract: %d %v %s", status, o.Class, RedactSecret(o.Reason, key))
			}
			if cost, ok := tv.ActualCost(op, body); ok {
				t.Logf("extract ok: %d credit(s); fields: %s", cost.Credits, topFields(body))
			} else {
				t.Errorf("extract: no usage.credits (fields: %s)", topFields(body))
			}
			_ = injected

			req, _ := tv.UsageRequest(context.Background(), key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			ub, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("usage: %d", resp.StatusCode)
			}
			q, err := tv.ParseUsage(ub)
			if err != nil || q.Limit == nil || q.Used == nil {
				t.Fatalf("usage parse: %v %+v (key fields: %s / account fields: %s)", err, q,
					topFields([]byte(gjson.GetBytes(ub, "key").Raw)), topFields([]byte(gjson.GetBytes(ub, "account").Raw)))
			}
			t.Logf("usage ok: plan=%q used=%d limit=%d remaining=%d (key fields: %s; account fields: %s)",
				q.Plan, *q.Used, *q.Limit, *q.Remaining,
				topFields([]byte(gjson.GetBytes(ub, "key").Raw)), topFields([]byte(gjson.GetBytes(ub, "account").Raw)))
		})
	}
}

func TestLiveExa(t *testing.T) {
	ex := NewExa("")
	for _, key := range liveKeys(t, "WARDEN_LIVE_EXA_KEYS") {
		t.Run(mask(key), func(t *testing.T) {
			op, status, h, body, _ := call(t, ex, key, "POST", "/search", `{"query":"search-warden live test","numResults":1}`, true)
			o := ex.Classify(op, status, h, body)
			if o.Class != ClassOK {
				t.Fatalf("search: %d %v %s", status, o.Class, RedactSecret(o.Reason, key))
			}
			cost, ok := ex.ActualCost(op, body)
			if !ok {
				t.Fatalf("search: no costDollars.total (fields: %s)", topFields(body))
			}
			t.Logf("search ok: $%.4f; fields: %s; costDollars: %s", float64(cost.MicroUSD)/1e6, topFields(body), gjson.GetBytes(body, "costDollars").Raw)

			op, status, h, body, _ = call(t, ex, key, "POST", "/contents", `{"urls":["https://example.com"],"text":true}`, true)
			if o := ex.Classify(op, status, h, body); o.Class != ClassOK {
				t.Fatalf("contents: %d %v %s", status, o.Class, RedactSecret(o.Reason, key))
			}
			if cost, ok := ex.ActualCost(op, body); ok {
				t.Logf("contents ok: $%.4f; fields: %s", float64(cost.MicroUSD)/1e6, topFields(body))
			} else {
				t.Errorf("contents: no costDollars (fields: %s)", topFields(body))
			}
		})
	}
}

// TestLiveInvalidKeys needs no real key; it pins how upstream rejects bad credentials.
func TestLiveInvalidKeys(t *testing.T) {
	for _, c := range []struct {
		p    Provider
		key  string
		path string
		body string
	}{
		{NewTavily(""), "tvly-invalid-0000000000", "/search", `{"query":"x","max_results":1}`},
		{NewExa(""), "00000000-0000-0000-0000-000000000000", "/search", `{"query":"x","numResults":1}`},
	} {
		op, status, h, body, _ := call(t, c.p, c.key, "POST", c.path, c.body, false)
		o := c.p.Classify(op, status, h, body)
		if o.Class != ClassKeyInvalid {
			t.Errorf("%s: %d → %v (%s)", c.p.ID(), status, o.Class, o.Reason)
		} else {
			t.Logf("%s invalid key → %d %v: %s", c.p.ID(), status, o.Class, o.Reason)
		}
	}
}
