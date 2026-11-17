package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// pagesEnv is a logged-in panel backed by a real key pool and egress runtime.
type pagesEnv struct {
	*env
	c      *http.Client
	pool   *pool.Pool
	out    *outbound.Runtime
	probed []string
	synced []string
	tested []string
}

func newPagesEnv(t *testing.T) *pagesEnv {
	pe := &pagesEnv{}
	pe.env = newEnv(t, func(o *Options) {
		o.Keys = func() []pool.View { return pe.pool.Views() }
		o.Egresses = func() []outbound.View { return pe.out.Views() }
		o.ProbeEgress = func(ids ...string) { pe.probed = append(pe.probed, ids...) }
		o.SyncQuota = func(ids ...string) int { pe.synced = append(pe.synced, ids...); return len(ids) }
		o.TestKeys = func(ids ...string) int { pe.tested = append(pe.tested, ids...); return len(ids) }
	})
	pe.out = outbound.New(outbound.Options{})
	pe.pool = pool.New(pool.Options{EgressUsable: pe.out.Usable})
	apply := func(c confstore.Change) { pe.out.Apply(c.New); pe.pool.Apply(c.New) }
	pe.conf.Subscribe(apply)
	apply(confstore.Change{New: pe.conf.Snapshot()})
	pe.c = pe.browser()
	if code := pe.login(pe.c, pw, ""); code != http.StatusSeeOther {
		t.Fatalf("login = %d", code)
	}
	return pe
}

// page GETs a page and fails unless it is 200.
func (pe *pagesEnv) page(path string) string {
	pe.t.Helper()
	code, body, _ := pe.get(pe.c, path)
	if code != http.StatusOK {
		pe.t.Fatalf("GET %s = %d\n%s", path, code, body)
	}
	return body
}

// htmxSubmit POSTs a dialog form (HX-Request), so the handler answers with a fragment.
func (pe *pagesEnv) htmxSubmit(from, path string, form url.Values) (int, string, http.Header) {
	pe.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", pe.csrf(pe.c, from))
	return pe.htmxPost(pe.c, path, form)
}

// submit POSTs a form with the CSRF token of the page it came from.
func (pe *pagesEnv) submit(from, path string, form url.Values) (int, string, http.Header) {
	pe.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", pe.csrf(pe.c, from))
	return pe.post(pe.c, path, form)
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Fatalf("page lacks %q", w)
		}
	}
}

// idRe finds a key id in a link or in the button that opens the key dialog.
var idRe = regexp.MustCompile(`(?:href|data-modal)="[^"]*/keys/([0-9a-f]{12})"`)

func TestKeysPage(t *testing.T) {
	pe := newPagesEnv(t)
	// Adding keys from the dialog redirects to the list, which closes the modal.
	code, _, hdr := pe.submit("/keys", "/keys/add", url.Values{"lines": {"tvly-dev-aaaaaaaaaaaaaaaa,free-01\nexa-key-bbbbbbbbbbbbbbbb,team-exa,,paid\n# comment\ntvly-dev-aaaaaaaaaaaaaaaa"}})
	if code != http.StatusSeeOther || hdr.Get("Location") != "/keys?ok=keys_added" {
		t.Fatalf("add = %d %s", code, hdr.Get("Location"))
	}
	// The same endpoint takes the single-key form: a key plus the pickers.
	if code, _, hdr := pe.submit("/keys/new", "/keys/add", url.Values{"secret": {"tvly-dev-cccccccccccccccc"}, "label": {"single-01"}, "plan": {"paid"}, "egress": {"direct"}}); code != http.StatusSeeOther || hdr.Get("Location") != "/keys?ok=keys_added" {
		t.Fatalf("single-key add = %d %s", code, hdr.Get("Location"))
	}
	// A dialog request gets the fragment only, not a whole page.
	req := httptest.NewRequest(http.MethodGet, "/keys/new", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "warden_admin", Value: pe.secret()})
	rec := httptest.NewRecorder()
	pe.p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "<!doctype html>") || !strings.Contains(rec.Body.String(), `name="secret"`) {
		t.Fatalf("dialog fragment = %d %.200s", rec.Code, rec.Body.String())
	}
	if len(pe.synced) == 0 {
		t.Error("new Tavily keys were not queued for a quota sync")
	}
	body := pe.page("/keys")
	mustContain(t, body, "free-01", "single-01", "team-exa", "付费", "可用")
	if strings.Contains(body, "aaaaaaaaaaaaaaaa") {
		t.Fatal("a full key is on the keys page")
	}
	ids := idRe.FindAllStringSubmatch(body, -1)
	if len(ids) != 3 {
		t.Fatalf("key rows = %v", ids)
	}
	tv := ids[0][1]

	// Bad input is reported, not written.
	if code, body, _ := pe.submit("/keys", "/keys/add", url.Values{"lines": {"a,b,c,d,e"}}); code != http.StatusBadRequest || !strings.Contains(body, "格式错误") {
		t.Fatalf("bad line = %d", code)
	}

	// Batch: disabling needs an explicit selection; setting an egress needs a choice.
	if code, _, _ := pe.submit("/keys", "/keys/batch", url.Values{"action": {"disable"}}); code != http.StatusBadRequest {
		t.Fatal("batch without a selection accepted")
	}
	if code, body, _ := pe.submit("/keys", "/keys/batch", url.Values{"action": {"egress"}, "id": {tv}, "egress": {"__none__"}}); code != http.StatusBadRequest || !strings.Contains(body, "请选择出口") {
		t.Fatal("egress without a choice accepted")
	}
	if code, _, h := pe.submit("/keys", "/keys/batch", url.Values{"action": {"disable"}, "id": {tv}}); code != http.StatusSeeOther || h.Get("Location") != "/keys?ok=keys_updated" {
		t.Fatalf("disable = %d %v", code, h)
	}
	if k, _ := pe.conf.Snapshot().KeyByID(tv); k.Enabled {
		t.Fatal("not disabled")
	}
	if code, _, _ := pe.submit("/keys", "/keys/batch", url.Values{"action": {"egress"}, "id": {tv}, "egress_url": {"socks5://u:p@10.1.2.3:1080"}}); code != http.StatusSeeOther {
		t.Fatal("inline egress not set")
	}

	// The key page never puts a proxy password in the page, and saving keeps the inline egress.
	body = pe.page("/keys/" + tv)
	mustContain(t, body, "保持当前手填的出口", "u:***@10.1.2.3")
	if strings.Contains(body, "u:p@") {
		t.Fatal("proxy password on the key page")
	}
	form := url.Values{"label": {"renamed"}, "plan": {"paid"}, "egress": {"__keep__"}, "priority": {"2"}, "reset_day": {"5"}, "budget_monthly": {"500"}, "rate_limit": {""}, "enabled": {"on"}}
	if code, _, _ := pe.submit("/keys/"+tv, "/keys/"+tv, form); code != http.StatusSeeOther {
		t.Fatalf("save = %d", code)
	}
	k, _ := pe.conf.Snapshot().KeyByID(tv)
	if k.Label != "renamed" || k.Plan != "paid" || k.Priority != 2 || k.ResetDay != 5 || k.BudgetMonthly != 500 || !k.Enabled || !k.Egress.Inline {
		t.Fatalf("saved key = %+v", k)
	}
	if code, _, h := pe.submit("/keys", "/keys/batch", url.Values{"action": {"test"}, "id": {tv}}); code != http.StatusSeeOther ||
		h.Get("Location") != "/keys?ok=testing_keys" || len(pe.tested) != 1 {
		t.Fatalf("test action = %d %v %v", code, h, pe.tested)
	}
	form.Set("weight", "2.5")
	if code, _, _ := pe.submit("/keys/"+tv, "/keys/"+tv, form); code != http.StatusSeeOther {
		t.Fatal("weight not saved")
	}
	if k, _ := pe.conf.Snapshot().KeyByID(tv); k.Weight != 2.5 {
		t.Fatalf("weight = %v", k.Weight)
	}
	mustContain(t, pe.page("/keys"), "权重 2.5")
	form.Set("weight", "0")
	if code, body, _ := pe.submit("/keys/"+tv, "/keys/"+tv, form); code != http.StatusBadRequest || !strings.Contains(body, "权重") {
		t.Fatal("weight 0 accepted")
	}
	form.Set("weight", "2.5")
	form.Set("priority", "x")
	if code, body, _ := pe.submit("/keys/"+tv, "/keys/"+tv, form); code != http.StatusBadRequest || !strings.Contains(body, "优先级") {
		t.Fatal("bad priority accepted")
	}

	// Revealing the full key is audited.
	if _, body, _ := pe.submit("/keys/"+tv, "/keys/"+tv+"/reveal", nil); !strings.Contains(body, "tvly-dev-aaaaaaaaaaaaaaaa") {
		t.Fatal("reveal did not show the key")
	}
	if a, _ := pe.st.ListAudit(context.Background(), store.AuditQuery{Action: "key.reveal"}); len(a) != 1 || strings.Contains(a[0].Target, "aaaaaaaa") {
		t.Fatalf("audit = %+v", a)
	}

	if code, _, _ := pe.submit("/keys", "/keys/batch", url.Values{"action": {"delete"}, "id": {tv}}); code != http.StatusSeeOther || len(pe.conf.Snapshot().Keys) != 2 {
		t.Fatal("delete failed")
	}
	if code, _, _ := pe.get(pe.c, "/keys/"+tv); code != http.StatusNotFound {
		t.Fatal("deleted key page still served")
	}
}

func TestEgressPages(t *testing.T) {
	pe := newPagesEnv(t)
	if _, _, err := pe.conf.AddKeys(nil, []confstore.KeyInput{{Secret: "tvly-dev-cccccccccccccccc", Egress: "socks5://u:secret@10.9.9.9:1080"}}); err != nil {
		t.Fatal(err)
	}
	body := pe.page("/egresses")
	mustContain(t, body, "未保存的手填出口", "u:***@10.9.9.9")
	if strings.Contains(body, "secret") {
		t.Fatal("proxy password on the egress page")
	}

	// Preview first, nothing written; then confirm.
	text := "10.0.0.1:1080 #hk-1\n10.0.0.2:1080:user:pw\nnot a proxy\n10.0.0.1:1080"
	code, body, _ := pe.htmxSubmit("/egresses", "/egresses/import", url.Values{"text": {text}, "scheme": {"socks5"}, "prefix": {"hk-"}, "step": {"preview"}, "absorb": {"on"}})
	if code != http.StatusOK || len(pe.conf.Snapshot().Egresses.Nodes()) != 0 {
		t.Fatalf("preview = %d, nodes %d", code, len(pe.conf.Snapshot().Egresses.Nodes()))
	}
	mustContain(t, body, "将新增 <strong>2</strong> 个节点", "格式错误", "重复，跳过", "user:***@10.0.0.2")
	// The dialog opens as a fragment.
	if code, frag, _ := pe.htmxGet(pe.c, "/egresses/import"); code != http.StatusOK || strings.Contains(frag, "<!doctype html>") || !strings.Contains(frag, `name="text"`) {
		t.Fatalf("import dialog = %d %.160s", code, frag)
	}
	code, _, h := pe.submit("/egresses", "/egresses/import", url.Values{"text": {text}, "scheme": {"socks5"}, "prefix": {"hk-"}, "region": {"HK"}, "step": {"confirm"}})
	if code != http.StatusSeeOther || h.Get("Location") != "/egresses?ok=imported" || len(pe.conf.Snapshot().Egresses.Nodes()) != 2 || len(pe.probed) != 2 {
		t.Fatalf("import = %d %v nodes=%d probed=%v", code, h, len(pe.conf.Snapshot().Egresses.Nodes()), pe.probed)
	}

	// "Save as node" for the inline proxy: the URL is looked up server-side and the key adopts the node.
	var inlineID string
	for _, k := range pe.conf.Snapshot().Keys {
		inlineID = k.Egress.ID
	}
	if code, body, _ := pe.submit("/egresses", "/egresses/import", url.Values{"inline_id": {inlineID}, "name": {"res-1"}}); code != http.StatusSeeOther {
		t.Fatalf("save inline as node failed: %d %s", code, regexp.MustCompile(`flash err[^<]*<`).FindString(body))
	}
	if k := pe.conf.Snapshot().Keys[0]; k.EgressRef != "res-1" {
		t.Fatalf("key egress = %q", k.EgressRef)
	}

	// Edit, rename (references follow), delete (keys must be handled).
	body = pe.page("/egresses/res-1")
	mustContain(t, body, "u:***@10.9.9.9", "tavily · ")
	if code, _, _ := pe.submit("/egresses/res-1", "/egresses/res-1", url.Values{"max_keys": {"3"}, "tags": {"a, b"}, "region": {"US"}, "enabled": {"on"}}); code != http.StatusSeeOther {
		t.Fatal("egress save failed")
	}
	if n, _ := pe.conf.Snapshot().Egresses.Lookup("res-1"); n.MaxKeys != 3 || len(n.Tags) != 2 || !strings.Contains(n.URL, "secret") {
		t.Fatalf("node = %+v", n) // a blank URL field keeps the URL and its password
	}
	if code, _, _ := pe.submit("/egresses/res-1", "/egresses/res-1/rename", url.Values{"to": {"res-2"}}); code != http.StatusSeeOther || pe.conf.Snapshot().Keys[0].EgressRef != "res-2" {
		t.Fatal("rename did not follow the key reference")
	}
	if code, body, _ := pe.submit("/egresses/res-2", "/egresses/res-2/delete", url.Values{}); code != http.StatusBadRequest || !strings.Contains(body, "还有 1 个 key") {
		t.Fatalf("delete of a used node = %d", code)
	}
	if code, _, _ := pe.submit("/egresses/res-2", "/egresses/res-2/delete", url.Values{"mode": {"inline"}}); code != http.StatusSeeOther {
		t.Fatal("delete with inline mode failed")
	}
	if k := pe.conf.Snapshot().Keys[0]; !k.Egress.Inline || !strings.Contains(k.Egress.URL, "10.9.9.9") {
		t.Fatalf("key after delete = %+v", k.Egress)
	}

	// Export carries passwords: audited.
	code, body, h = pe.submit("/egresses", "/egresses/export", nil)
	if code != 200 || !strings.Contains(body, "user:pw@10.0.0.2") || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("export = %d", code)
	}
	if a, _ := pe.st.ListAudit(context.Background(), store.AuditQuery{Action: "egress.export"}); len(a) != 1 {
		t.Fatal("export not audited")
	}
}

func TestTokenPages(t *testing.T) {
	pe := newPagesEnv(t)
	form := url.Values{"name": {"laptop"}, "providers": {"tavily", "exa"}, "surfaces": {"mcp"}, "key_group": {"free"},
		"tools": {"tavily_search, web_search_exa"}, "limit_rpm": {"60"}, "limit_monthly_usd": {"1.5"}, "expires": {"2030-01-01"}, "enabled": {"on"}}
	code, body, _ := pe.submit("/tokens", "/tokens/add", form)
	if code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	secret := regexp.MustCompile(`sw_[A-Za-z0-9_]+`).FindString(body)
	tk, ok := pe.conf.Snapshot().TokenByHash(token.Hash(secret))
	if !ok || tk.Name != "laptop" || len(tk.Surfaces) != 1 || tk.Limits.RPM != 60 || tk.Limits.MonthlyUSD != 1.5 || len(tk.Tools) != 2 || tk.KeyGroup != "free" {
		t.Fatalf("token = %+v (secret found: %v)", tk, secret != "")
	}
	if list := pe.page("/tokens"); strings.Contains(list, secret) || !strings.Contains(list, tk.Prefix) {
		t.Fatal("the list shows the secret, or not the prefix")
	}
	for name, f := range map[string]url.Values{
		"no provider": {"name": {"x"}, "surfaces": {"mcp"}},
		"bad tool":    {"name": {"x"}, "providers": {"exa"}, "surfaces": {"mcp"}, "tools": {"rm_rf"}},
		"bad limit":   {"name": {"x"}, "providers": {"exa"}, "surfaces": {"mcp"}, "limit_rpm": {"-1"}},
		"duplicate":   {"name": {"Laptop"}, "providers": {"exa"}, "surfaces": {"mcp"}},
	} {
		if code, _, _ := pe.submit("/tokens", "/tokens/add", f); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}

	page := "/tokens/" + tk.ID
	mustContain(t, pe.page(page), "laptop", "2030-01-01")
	form.Set("limit_rpm", "")
	form.Set("tools", "")
	if code, _, _ := pe.submit(page, page, form); code != http.StatusSeeOther {
		t.Fatal("edit failed")
	}
	if tk2, _ := pe.conf.Snapshot().TokenByID(tk.ID); tk2.Limits.RPM != 0 || len(tk2.Tools) != 0 {
		t.Fatalf("edited = %+v", tk2)
	}
	if code, _, h := pe.submit(page, page+"/toggle", nil); code != http.StatusSeeOther || !strings.HasSuffix(h.Get("Location"), "ok=token_off") {
		t.Fatal("toggle failed")
	}
	code, body, _ = pe.submit(page, page+"/rotate", nil)
	newSecret := regexp.MustCompile(`sw_[A-Za-z0-9_]+`).FindString(body)
	if code != 200 || newSecret == "" || newSecret == secret {
		t.Fatal("rotate did not show a new secret")
	}
	if _, ok := pe.conf.Snapshot().TokenByHash(token.Hash(secret)); ok {
		t.Fatal("old secret still valid")
	}
	rotated, _ := pe.conf.Snapshot().TokenByHash(token.Hash(newSecret))
	if code, _, _ := pe.submit("/tokens/"+rotated.ID, "/tokens/"+rotated.ID+"/delete", nil); code != http.StatusSeeOther || len(pe.conf.Snapshot().Tokens) != 0 {
		t.Fatal("delete failed")
	}
}

func TestSettingsPage(t *testing.T) {
	pe := newPagesEnv(t)
	body := pe.page("/settings")
	mustContain(t, body, "选 key 与重试", "额度同步", `name="routing__max_attempts" value="3"`)
	form := url.Values{"routing__strategy": {"round-robin"}, "routing__max_attempts": {"5"}, "routing__deadline_seconds": {"180"},
		"routing__affinity_minutes": {"30"}, "routing__per_key_rpm": {"0"}, "routing__per_key_concurrency": {"0"}}
	if code, _, h := pe.submit("/settings", "/settings/routing", form); code != http.StatusSeeOther || h.Get("Location") != "/settings?ok=saved#routing" {
		t.Fatalf("save = %d %v", code, h)
	}
	c := pe.conf.Snapshot().Config
	if c.Routing.Strategy != "round-robin" || c.Routing.MaxAttempts != 5 {
		t.Fatalf("routing = %+v", c.Routing)
	}
	// Out of range: nothing is written, the input is kept.
	form.Set("routing__max_attempts", "99")
	form.Set("routing__strategy", "balanced")
	code, body, _ := pe.submit("/settings", "/settings/routing", form)
	if code != http.StatusBadRequest || !strings.Contains(body, `value="99"`) || pe.conf.Snapshot().Config.Routing.Strategy != "round-robin" {
		t.Fatalf("invalid save = %d", code)
	}
	// Checkboxes and lists.
	qf := url.Values{"quota__hot_interval_minutes": {"30"}, "quota__cold_interval_hours": {"24"}, "quota__exa_free_monthly_usd": {"10"}}
	if code, _, _ := pe.submit("/settings", "/settings/quota", qf); code != http.StatusSeeOther || pe.conf.Snapshot().Config.Quota.Sync {
		t.Fatal("unchecked box not saved as false")
	}
	nf := url.Values{"network__trusted_proxies": {"10.0.0.0/8, 192.168.0.0/16"}, "network__client_ip_headers": {"X-Forwarded-For"}}
	if code, _, _ := pe.submit("/settings", "/settings/network", nf); code != http.StatusSeeOther || len(pe.conf.Snapshot().Config.Network.TrustedProxies) != 2 {
		t.Fatal("list not saved")
	}
	if code, _, _ := pe.submit("/settings", "/settings/nope", nil); code != http.StatusNotFound {
		t.Fatal("unknown group")
	}
}

func TestFilesPages(t *testing.T) {
	pe := newPagesEnv(t)
	mustContain(t, pe.page("/files"), "config.yaml", "keys/tavily.yaml", "tokens.yaml")
	body := pe.page("/files/edit?f=keys/exa.yaml")
	ver := regexp.MustCompile(`name="version" value="([^"]+)"`).FindStringSubmatch(body)[1]

	// A broken file is rejected with its line number and nothing changes.
	code, body, _ := pe.submit("/files/edit?f=keys/exa.yaml", "/files/edit?f=keys/exa.yaml", url.Values{"version": {ver}, "content": {"keys:\r\n  - key: exa-1\r\n    plan: gold\r\n"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "plan") {
		t.Fatalf("bad save = %d", code)
	}
	code, _, _ = pe.submit("/files/edit?f=keys/exa.yaml", "/files/edit?f=keys/exa.yaml", url.Values{"version": {ver}, "content": {"keys:\r\n  - key: exa-key-dddddddddddd\r\n"}})
	if code != http.StatusSeeOther || len(pe.conf.Snapshot().KeysFor("exa")) != 1 {
		t.Fatalf("save = %d", code)
	}
	if raw, _, _ := pe.conf.ReadRaw("keys/exa.yaml"); bytes.Contains(raw, []byte("\r\n")) {
		t.Fatal("CRLF from the browser written into an LF file")
	}
	// Saving on top of an old version is a conflict.
	code, body, _ = pe.submit("/files/edit?f=keys/exa.yaml", "/files/edit?f=keys/exa.yaml", url.Values{"version": {ver}, "content": {"keys: []\n"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "刚被其他人或手工修改过") {
		t.Fatalf("conflict = %d", code)
	}
	if code, _, _ := pe.get(pe.c, "/files/edit?f=../etc/passwd"); code != http.StatusNotFound {
		t.Fatal("unmanaged file served")
	}

	code, body, h := pe.submit("/files", "/files/export", nil)
	if code != 200 || h.Get("Content-Type") != "application/zip" {
		t.Fatalf("export = %d %v", code, h)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil || len(zr.File) != len(confstore.AllFiles()) {
		t.Fatalf("zip: %v", err)
	}
	if a, _ := pe.st.ListAudit(context.Background(), store.AuditQuery{Action: "config.export"}); len(a) != 1 {
		t.Fatal("export not audited")
	}
	mustContain(t, pe.page("/security?source=panel"), "config.export")
}

func TestDashboardAndRequests(t *testing.T) {
	pe := newPagesEnv(t)
	if _, _, err := pe.conf.AddKeys(nil, []confstore.KeyInput{{Secret: "tvly-dev-eeeeeeeeeeeeeeee", Label: "free-9"}}); err != nil {
		t.Fatal(err)
	}
	kid := pe.conf.Snapshot().Keys[0].ID
	now := time.Now()
	logs := []store.RequestLog{
		{TS: now.Add(-time.Hour), RequestID: "r1", Provider: "tavily", Surface: "mcp", Operation: "search", KeyID: kid, ClientIP: "203.0.113.5", IPSource: "X-Forwarded-For",
			Status: 200, Outcome: "ok", LatencyMS: 120, CostCredits: 1, QueryPreview: "<script>alert(1)</script>"},
		{TS: now.Add(-30 * time.Minute), RequestID: "r2", Provider: "exa", Surface: "rest", Operation: "search", ClientIP: "203.0.113.6",
			Status: 503, Outcome: "gateway_error", ErrorKind: "no_keys", LatencyMS: 3, Attempts: 2},
	}
	if err := pe.st.InsertRequestLogs(context.Background(), logs, []store.Attempt{{RequestID: "r2", N: 1, TS: now, KeyID: "gone", Status: 502}, {RequestID: "r2", N: 2, TS: now, Status: 503}}); err != nil {
		t.Fatal(err)
	}
	body := pe.page("/?range=7d")
	mustContain(t, body, "<svg", "s-tavily", "tavily/search", "203.0.113.5", "无可用 key", "free-9", "50%")
	if strings.Contains(body, "<script>alert") {
		t.Fatal("unescaped log content")
	}

	body = pe.page("/requests")
	mustContain(t, body, "203.0.113.6", "no_keys", "&lt;script&gt;")
	body = pe.page("/requests?errors=1")
	if strings.Contains(body, "203.0.113.5") || !strings.Contains(body, "203.0.113.6") {
		t.Fatal("error filter")
	}
	if body := pe.page("/requests?key=" + kid); !strings.Contains(body, "203.0.113.5") || strings.Contains(body, "203.0.113.6") {
		t.Fatal("key filter")
	}
	live := pe.page("/requests?live=1")
	mustContain(t, live, `hx-trigger="every 3s"`)
	rows := pe.page("/requests/rows?live=1")
	if strings.Contains(rows, "<html") || !strings.Contains(rows, `<tbody id="rows"`) {
		t.Fatal("rows fragment is not a fragment")
	}
	all := pe.page("/requests")
	id := regexp.MustCompile(`href="/requests/(\d+)"`).FindAllStringSubmatch(all, -1)
	if len(id) != 2 {
		t.Fatalf("detail links = %v", id)
	}
	detail := pe.page("/requests/" + id[0][1]) // newest first: the failed one, with two attempts
	mustContain(t, detail, "尝试记录", "502", "gateway_error")
	if code, _, _ := pe.get(pe.c, "/requests/999999"); code != http.StatusNotFound {
		t.Fatal("missing request")
	}
}

func TestChartAndHelpers(t *testing.T) {
	from := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	svg := string(chartSVG(langZH, []store.SeriesPoint{{Bucket: from, Provider: "tavily", Requests: 8, Errors: 2}, {Bucket: from.Add(time.Hour), Provider: "exa", Requests: 3}},
		from, from.Add(2*time.Hour), time.Hour))
	if !strings.Contains(svg, `class="s-tavily"`) || !strings.Contains(svg, `class="s-exa"`) || !strings.Contains(svg, "polyline") || strings.Contains(svg, "style=") {
		t.Fatalf("svg = %s", svg)
	}
	// The plot stretches to its card, so no text may be inside it: the labels are HTML around it.
	if !strings.Contains(svg, `preserveAspectRatio="none"`) || strings.Contains(svg, "<text") ||
		!strings.Contains(svg, `<div class="chart-y"><span>10</span><span>5</span><span>0</span></div>`) || strings.Count(svg, `<div class="chart-x"><span>`) != 1 {
		t.Fatalf("chart layout = %s", svg)
	}
	if s := string(chartSVG(langZH, nil, from, from.Add(time.Hour), time.Hour)); !strings.Contains(s, "没有请求") {
		t.Fatal(s)
	}
	if s := string(chartSVG(langEN, nil, from, from.Add(time.Hour), time.Hour)); !strings.Contains(s, "No requests") {
		t.Fatal(s)
	}
	for in, want := range map[int64]int64{0: 1, 1: 1, 7: 10, 11: 20, 150: 200, 600: 1000} {
		if got := niceCeil(in); got != want {
			t.Errorf("niceCeil(%d) = %d, want %d", in, got, want)
		}
	}
	if pct(1, 3) != "33.3%" || pct(0, 0) != "—" || pct(1, 10000) != "<0.1%" {
		t.Error("pct")
	}
	if msText(950) != "950 ms" || msText(12_345) != "12.3 s" {
		t.Error("ms")
	}
}

// secret is the session cookie value of the page env's client.
func (pe *pagesEnv) secret() string {
	pe.t.Helper()
	for _, c := range pe.c.Jar.Cookies(mustURL(pe.t, pe.srv.URL)) {
		if c.Name == "warden_admin" {
			return c.Value
		}
	}
	pe.t.Fatal("no session cookie")
	return ""
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
