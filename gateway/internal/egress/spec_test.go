package egress

import (
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		"socks5://1.2.3.4:1080":             "socks5://1.2.3.4",
		"SOCKS5H://Proxy.Example.COM:2080/": "socks5://proxy.example.com:2080",
		"socks5h://u:p@1.2.3.4:01081":       "socks5://u:p@1.2.3.4:1081",
		"http://u:p%40ss@5.6.7.8:3128":      "http://u:p%40ss@5.6.7.8:3128",
		"http://5.6.7.8:80":                 "http://5.6.7.8",
		"https://proxy.example.com:443":     "https://proxy.example.com",
		"socks5://[2001:DB8::1]:1080":       "socks5://[2001:db8::1]",
		"socks5://[2001:db8::1]:9050":       "socks5://[2001:db8::1]:9050",
		"  socks5://h:1  ":                  "socks5://h:1",
	}
	for in, want := range ok {
		got, err := NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ftp://h:1", "socks4://h:1", "socks5://", "socks5://h:0", "socks5://h:70000",
		"http://h:1/path", "http://h:1?x=1", "1.2.3.4:1080", "socks5://u:p@:1080"} {
		if _, err := NormalizeURL(bad); err == nil {
			t.Errorf("NormalizeURL(%q) should fail", bad)
		}
	}
}

func TestNormalizeErrorsHideCredentials(t *testing.T) {
	_, err := NormalizeURL("socks5://user:TOPSECRET@h:1/%zz")
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestIDStableAcrossEquivalentSpellings(t *testing.T) {
	a, _ := NormalizeURL("socks5h://U:p@HOST:1080")
	b, _ := NormalizeURL("socks5://U:p@host")
	if ID(a) != ID(b) {
		t.Fatalf("equivalent URLs got different IDs: %s %s", ID(a), ID(b))
	}
	c, _ := NormalizeURL("socks5://U:other@host")
	if ID(a) == ID(c) {
		t.Fatal("different credentials must be different egresses")
	}
	if ID("") != DirectID || !strings.HasPrefix(ID(a), "e") || len(ID(a)) != 13 {
		t.Fatalf("ID format: %q", ID(a))
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("socks5://user:pw@1.2.3.4:1080"); got != "socks5://user:***@1.2.3.4:1080" {
		t.Errorf("Redact = %q", got)
	}
	if got := Redact("http://1.2.3.4:3128"); got != "http://1.2.3.4:3128" {
		t.Errorf("Redact no-cred = %q", got)
	}
}

func TestValidateName(t *testing.T) {
	for _, good := range []string{"hk-res-1", "US_1", "a", "node.01"} {
		if err := ValidateName(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"", "direct", "DIRECT", "socks5://x", "-x", "has space", "1.2.3.4", "::1", strings.Repeat("a", 65), "a:b"} {
		if ValidateName(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in   string
		kind RefKind
	}{{"", RefDirect}, {"direct", RefDirect}, {" Direct ", RefDirect}, {"hk-1", RefNamed}, {"socks5://h:1", RefInline}}
	for _, c := range cases {
		r, err := ParseRef(c.in)
		if err != nil || r.Kind != c.kind {
			t.Errorf("ParseRef(%q) = %+v, %v", c.in, r, err)
		}
	}
	_, err := ParseRef("1.2.3.4:1080")
	if err == nil || !strings.Contains(err.Error(), "socks5://1.2.3.4:1080") {
		t.Errorf("bare host:port should get a scheme hint, got %v", err)
	}
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, errs := NewRegistry([]Node{
		{Name: "hk-1", URL: "socks5://u:p@1.1.1.1:1080", Enabled: true, MaxKeys: 1, Region: "HK"},
		{Name: "us-1", URL: "http://2.2.2.2:3128", Enabled: false},
	})
	if errs != nil {
		t.Fatal(errs)
	}
	return r
}

func TestNewRegistryRejectsConflicts(t *testing.T) {
	_, errs := NewRegistry([]Node{
		{Name: "a", URL: "socks5://h:1"},
		{Name: "A", URL: "socks5://h:2"},              // duplicate name (case-insensitive)
		{Name: "b", URL: "socks5h://H:1"},             // same proxy as "a"
		{Name: "direct", URL: "socks5://h:3"},         // reserved
		{Name: "c", URL: "ftp://h:4"},                 // bad scheme
		{Name: "d", URL: "socks5://h:5", MaxKeys: -1}, // bad max_keys
	})
	if len(errs) != 5 {
		t.Fatalf("want 5 errors, got %d: %v", len(errs), errs)
	}
}

func TestResolveCompatibility(t *testing.T) {
	r := testRegistry(t)

	byName, err := r.Resolve("hk-1")
	if err != nil {
		t.Fatal(err)
	}
	// Same proxy written inline with a different spelling → same egress.
	inline, err := r.Resolve("socks5h://u:p@1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if inline.ID != byName.ID || inline.Name != "hk-1" || !inline.Inline || byName.Inline {
		t.Fatalf("inline vs named mismatch: %+v vs %+v", inline, byName)
	}
	if inline.Label() != "hk-1 (inline)" || byName.Label() != "hk-1" {
		t.Errorf("labels: %q %q", inline.Label(), byName.Label())
	}

	// Unsaved inline egress: own stable ID, no name.
	adhoc, err := r.Resolve("socks5://9.9.9.9:1080")
	if err != nil || adhoc.Name != "" || !adhoc.Inline || adhoc.ID == byName.ID || adhoc.ID == DirectID {
		t.Fatalf("adhoc: %+v %v", adhoc, err)
	}

	// Disabled node propagates Enabled=false (key is paused, never falls back to direct).
	us, _ := r.Resolve("us-1")
	if us.Enabled || us.Direct() {
		t.Errorf("disabled node: %+v", us)
	}

	d, _ := r.Resolve("")
	if !d.Direct() || d.Label() != "direct" {
		t.Errorf("direct: %+v", d)
	}

	if _, err := r.Resolve("missing"); err == nil || !strings.Contains(err.Error(), "egresses.yaml") {
		t.Errorf("unknown name: %v", err)
	}
}

func TestCanonicalRef(t *testing.T) {
	r := testRegistry(t)
	cases := map[string]string{
		"hk-1":                      "hk-1",
		"HK-1":                      "hk-1",
		"socks5://u:p@1.1.1.1:1080": "hk-1", // matched → written as the node name
		"1.1.1.1:1080:u:p":          "hk-1", // import format in the panel field
		"socks5://9.9.9.9":          "socks5://9.9.9.9",
		"9.9.9.9:1081":              "socks5://9.9.9.9:1081",
		"direct":                    "direct",
		"":                          "direct",
	}
	for in, want := range cases {
		got, err := r.CanonicalRef(in)
		if err != nil || got != want {
			t.Errorf("CanonicalRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := r.CanonicalRef("nope"); err == nil {
		t.Error("unknown name should fail")
	}
}

func TestCountAndInlineOnly(t *testing.T) {
	r := testRegistry(t)
	vals := []string{"hk-1", "socks5://u:p@1.1.1.1", "", "socks5://9.9.9.9", "socks5h://9.9.9.9:1080", "bad name!"}
	u, errs := r.Count(vals)
	hk, _ := r.Resolve("hk-1")
	if u[hk.ID] != 2 || u[DirectID] != 1 || len(errs) != 1 {
		t.Fatalf("usage=%v errs=%v", u, errs)
	}
	if got := r.InlineOnly(vals); len(got) != 1 || got[0] != "socks5://9.9.9.9" {
		t.Errorf("InlineOnly = %v", got)
	}
}

func TestParseImportLine(t *testing.T) {
	cases := []struct{ in, scheme, url, name string }{
		{"socks5://u:p@1.2.3.4:1080", "", "socks5://u:p@1.2.3.4", ""},
		{"1.2.3.4:1080", "socks5", "socks5://1.2.3.4", ""},
		{"1.2.3.4:3128", "http", "http://1.2.3.4:3128", ""},
		{"1.2.3.4:1080:user:pa:ss", "socks5", "socks5://user:pa%3Ass@1.2.3.4", ""},
		{"user:pass@1.2.3.4:2000", "socks5", "socks5://user:pass@1.2.3.4:2000", ""},
		{"1.2.3.4:2000 #hk-9", "socks5", "socks5://1.2.3.4:2000", "hk-9"},
		{"socks5://1.2.3.4:2000|hk-8", "", "socks5://1.2.3.4:2000", "hk-8"},
		{"socks5://1.2.3.4:2000#hk-7", "", "socks5://1.2.3.4:2000", "hk-7"},
		{"[2001:db8::1]:1080", "socks5", "socks5://[2001:db8::1]", ""},
	}
	for _, c := range cases {
		got, err := ParseImportLine(c.in, c.scheme)
		if err != nil || got.URL != c.url || got.Name != c.name {
			t.Errorf("ParseImportLine(%q) = %+v, %v; want %s %s", c.in, got, err, c.url, c.name)
		}
	}
	for _, blank := range []string{"", "   ", "# comment"} {
		if c, err := ParseImportLine(blank, ""); err != nil || c.URL != "" {
			t.Errorf("blank %q: %+v %v", blank, c, err)
		}
	}
	for _, bad := range []string{"justtext", "1.2.3.4", "1.2.3.4:abc", "ftp://h:1", "1.2.3.4:1080 #direct"} {
		if _, err := ParseImportLine(bad, "socks5"); err == nil {
			t.Errorf("ParseImportLine(%q) should fail", bad)
		}
	}
}

func TestPlanImport(t *testing.T) {
	r, errs := NewRegistry([]Node{{Name: "node-3", URL: "socks5://1.1.1.1:1080", Enabled: true}})
	if errs != nil {
		t.Fatal(errs)
	}
	keyValues := []string{"socks5://4.4.4.4:1080", "node-3"}
	text := strings.Join([]string{
		"# HK batch",
		"1.1.1.1:1080",             // 2 duplicate of node-3
		"2.2.2.2:1080",             // 3 new → node-4
		"2.2.2.2:1080",             // 4 duplicate within paste
		"3.3.3.3:1080 #node-3",     // 5 name taken
		"4.4.4.4:1080",             // 6 absorbs a key's inline URL → node-5
		"garbage",                  // 7 invalid
		"5.5.5.5:1080:u:p #custom", // 8 new with explicit name
		"",
	}, "\r\n")
	rows := PlanImport(r, text, keyValues, ImportOptions{})
	want := []struct {
		line   int
		status ImportStatus
		name   string
	}{
		{2, ImportDuplicate, "node-3"},
		{3, ImportNew, "node-4"},
		{4, ImportDuplicate, ""},
		{5, ImportInvalid, ""},
		{6, ImportAbsorbs, "node-5"},
		{7, ImportInvalid, ""},
		{8, ImportNew, "custom"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows=%d: %+v", len(rows), rows)
	}
	for i, w := range want {
		if rows[i].Line != w.line || rows[i].Status != w.status || rows[i].Name != w.name {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
	for _, row := range rows {
		if strings.Contains(row.Error, ":p@") {
			t.Errorf("credentials in error: %q", row.Error)
		}
	}
}
