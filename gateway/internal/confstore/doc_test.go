package confstore

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func mustDoc(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := ParseDoc("t.yaml", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

// expectSurgical asserts the exact output and that no re-encode fallback happened.
func expectSurgical(t *testing.T, d *Doc, want string) {
	t.Helper()
	if !d.Surgical() {
		out, _ := d.reencoded()
		t.Fatalf("edit fell back to re-encode; re-encoded output:\n%s", out)
	}
	got, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

const keysSrc = `# Tavily keys
defaults:
  plan: free   # default plan

keys:
  - key: tvly-aaa   # first
    label: free-01

  # the second one
  - key: tvly-bbb
    egress: hk-1
  - tvly-ccc
`

func seqItem(t *testing.T, d *Doc, i int) (*yaml.Node, *yaml.Node) {
	t.Helper()
	seq := mapGet(d.Top(), "keys")
	return seq, seq.Content[i]
}

func TestSetFieldReplaceKeepsComments(t *testing.T) {
	d := mustDoc(t, keysSrc)
	_, item := seqItem(t, d, 0)
	d.SetField(item, "label", strNode("renamed key"))
	expectSurgical(t, d, strings.Replace(keysSrc, "label: free-01", "label: renamed key", 1))
}

func TestSetFieldQuotesWhenNeeded(t *testing.T) {
	d := mustDoc(t, keysSrc)
	_, item := seqItem(t, d, 1)
	d.SetField(item, "egress", strNode("true")) // must not become a boolean
	expectSurgical(t, d, strings.Replace(keysSrc, "egress: hk-1", `egress: 'true'`, 1))
}

func TestSetFieldAddsLineWithItemIndent(t *testing.T) {
	d := mustDoc(t, keysSrc)
	_, item := seqItem(t, d, 0)
	d.SetField(item, "egress", strNode("socks5://u:p@1.2.3.4:1080"))
	d.SetField(item, "enabled", boolNode(false))
	want := strings.Replace(keysSrc, "    label: free-01\n", "    label: free-01\n    egress: socks5://u:p@1.2.3.4:1080\n    enabled: false\n", 1)
	expectSurgical(t, d, want)
}

func TestShorthandItemBecomesMapping(t *testing.T) {
	d := mustDoc(t, keysSrc)
	seq, _ := seqItem(t, d, 2)
	m := d.ItemMapping(seq, 2, "key")
	d.SetField(m, "label", strNode("third"))
	want := strings.Replace(keysSrc, "  - tvly-ccc\n", "  - key: tvly-ccc\n    label: third\n", 1)
	expectSurgical(t, d, want)
}

func TestAppendItemToBlockSequence(t *testing.T) {
	d := mustDoc(t, keysSrc)
	d.AppendItem(d.Top(), "keys", mapNode("key", strNode("tvly-ddd"), "label", strNode("new"), "tags", flowList([]string{"a", "b"})))
	want := keysSrc + "  - key: tvly-ddd\n    label: new\n    tags: [a, b]\n"
	expectSurgical(t, d, want)
}

func TestAppendItemToNullAndEmptyFlow(t *testing.T) {
	src := "egresses:\n  # - name: example\n  #   url: socks5://h:1\nafter: 1\n"
	d := mustDoc(t, src)
	d.AppendItem(d.Top(), "egresses", mapNode("name", strNode("hk-1"), "url", strNode("socks5://1.1.1.1")))
	expectSurgical(t, d, "egresses:\n  - name: hk-1\n    url: socks5://1.1.1.1\n  # - name: example\n  #   url: socks5://h:1\nafter: 1\n")

	d = mustDoc(t, "keys: []   # none yet\nx: 1\n")
	d.AppendItem(d.Top(), "keys", strNode("tvly-x"))
	expectSurgical(t, d, "keys:\n  - tvly-x   # none yet\nx: 1\n")
}

func TestDeleteItemWithHeadComment(t *testing.T) {
	d := mustDoc(t, keysSrc)
	seq, _ := seqItem(t, d, 1)
	d.DeleteItem(seq, 1)
	want := strings.Replace(keysSrc, "  # the second one\n  - key: tvly-bbb\n    egress: hk-1\n", "", 1)
	expectSurgical(t, d, want)
}

func TestDeleteLastItemLeavesValidDoc(t *testing.T) {
	d := mustDoc(t, "keys:\n  - tvly-only\nother: 1\n")
	seq := mapGet(d.Top(), "keys")
	d.DeleteItem(seq, 0)
	expectSurgical(t, d, "keys:\nother: 1\n") // null value ≡ empty list
}

func TestDeleteAndRenameField(t *testing.T) {
	src := "admin:\n  username: admin\n  # plaintext, will be hashed\n  password: hunter2hunter2\n  session_max_days: 7\n"
	d := mustDoc(t, src)
	admin := mapGet(d.Top(), "admin")
	d.RenameField(admin, "password", "password_hash", strNode("$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"))
	expectSurgical(t, d, strings.Replace(src, "password: hunter2hunter2", "password_hash: $argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA", 1))

	d = mustDoc(t, src)
	admin = mapGet(d.Top(), "admin")
	d.DeleteField(admin, "password")
	expectSurgical(t, d, "admin:\n  username: admin\n  session_max_days: 7\n")
}

func TestSetFieldOnEmptyNullValue(t *testing.T) {
	d := mustDoc(t, "admin:\n  totp_secret:\n  x: 1\n")
	d.SetField(mapGet(d.Top(), "admin"), "totp_secret", strNode("JBSWY3DPEHPK3PXP"))
	expectSurgical(t, d, "admin:\n  totp_secret: JBSWY3DPEHPK3PXP\n  x: 1\n")
}

func TestReplaceFlowListValue(t *testing.T) {
	src := "network:\n  client_ip_headers: [X-Forwarded-For]   # keep me\n"
	d := mustDoc(t, src)
	d.SetField(mapGet(d.Top(), "network"), "client_ip_headers", flowList([]string{"X-Forwarded-For", "X-Real-IP"}))
	expectSurgical(t, d, "network:\n  client_ip_headers: [X-Forwarded-For, X-Real-IP]   # keep me\n")
}

func TestSetBlockFieldAddsSection(t *testing.T) {
	d := mustDoc(t, "# cfg\nrouting:\n  strategy: balanced\n")
	d.SetBlockField(d.Top(), "logs", mapNode("retention_days", intNode(14)))
	expectSurgical(t, d, "# cfg\nrouting:\n  strategy: balanced\nlogs:\n  retention_days: 14\n")
}

func TestCRLFPreserved(t *testing.T) {
	src := "keys:\r\n  - key: tvly-a\r\n    label: x\r\n"
	d := mustDoc(t, src)
	_, item := seqItem(t, d, 0)
	d.SetField(item, "note", strNode("n"))
	expectSurgical(t, d, "keys:\r\n  - key: tvly-a\r\n    label: x\r\n    note: n\r\n")
}

func TestMultiByteColumns(t *testing.T) {
	src := "keys:\n  - key: tvly-a\n    label: 中文标签   # 备注\n"
	d := mustDoc(t, src)
	_, item := seqItem(t, d, 0)
	d.SetField(item, "label", strNode("新标签"))
	expectSurgical(t, d, "keys:\n  - key: tvly-a\n    label: 新标签   # 备注\n")
}

func TestFallbackReencodeKeepsCommentsAndData(t *testing.T) {
	// A literal block scalar cannot be spliced; the engine must fall back safely.
	src := "keys:\n  - key: tvly-a\n    note: |\n      line1\n      line2\n# tail comment\n"
	d := mustDoc(t, src)
	_, item := seqItem(t, d, 0)
	d.SetField(item, "label", strNode("x"))
	if d.Surgical() {
		t.Fatal("expected fallback for literal block scalar")
	}
	out, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Keys []map[string]string `yaml:"keys"`
	}
	if err := yaml.Unmarshal(out, &v); err != nil || v.Keys[0]["label"] != "x" || v.Keys[0]["note"] != "line1\nline2\n" {
		t.Fatalf("fallback lost data: %v %+v\n%s", err, v, out)
	}
	if !strings.Contains(string(out), "# tail comment") {
		t.Errorf("fallback dropped comment:\n%s", out)
	}
}

func TestVerificationCatchesBadSplice(t *testing.T) {
	d := mustDoc(t, "a: 1\n")
	d.changed = true
	d.addSplice(0, 1, "b") // corrupt: renames key without touching the tree
	out, err := d.Bytes()
	if err != nil || string(out) != "a: 1\n" {
		t.Fatalf("verification should have rejected the splice; got %q %v", out, err)
	}
}

func TestOverlappingSplicesFallBack(t *testing.T) {
	d := mustDoc(t, "a: 1\n")
	d.changed = true
	d.addSplice(0, 3, "x")
	d.addSplice(1, 2, "y")
	if _, ok := d.applySplices(); ok {
		t.Fatal("overlap not detected")
	}
}

func TestEmptyDocument(t *testing.T) {
	d := mustDoc(t, "")
	top, err := d.ensureTop()
	if err != nil {
		t.Fatal(err)
	}
	d.SetField(top, "a", intNode(1))
	out, err := d.Bytes()
	if err != nil || strings.TrimSpace(string(out)) != "a: 1" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestSyntaxErrorHasLine(t *testing.T) {
	_, err := ParseDoc("x.yaml", []byte("a: 1\nb: [unclosed\nc: 2\n"))
	errs, ok := err.(Errors)
	if !ok || len(errs) != 1 || errs[0].Line == 0 || errs[0].File != "x.yaml" {
		t.Fatalf("err = %#v", err)
	}
}
