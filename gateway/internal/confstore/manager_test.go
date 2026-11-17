package confstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/password"
	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
)

const testPassword = "correct-horse-battery"

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// openTest creates a data dir from the templates with the admin password set via env.
func openTest(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := Open(dir, Options{AdminPasswordEnv: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

// recorder collects change notifications.
type recorder struct {
	mu sync.Mutex
	cs []Change
}

func (r *recorder) fn(c Change) { r.mu.Lock(); r.cs = append(r.cs, c); r.mu.Unlock() }
func (r *recorder) last() (Change, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cs) == 0 {
		return Change{}, 0
	}
	return r.cs[len(r.cs)-1], len(r.cs)
}

func statusOf(m *Manager, file string) FileStatus {
	for _, s := range m.Status() {
		if s.File == file {
			return s
		}
	}
	return FileStatus{}
}

func TestOpenRequiresAdminPassword(t *testing.T) {
	dir := t.TempDir()
	_, err := Open(dir, Options{})
	if err == nil || !strings.Contains(err.Error(), "admin password is not set") {
		t.Fatalf("err = %v", err)
	}
	// Templates were still written so the user can edit them.
	for _, f := range AllFiles() {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("template %s not created: %v", f, err)
		}
	}
}

func TestOpenBootstrapsPasswordFromEnv(t *testing.T) {
	m, dir := openTest(t)
	cfg := read(t, dir, FileConfig)
	if strings.Contains(cfg, testPassword) || !strings.Contains(cfg, "password_hash: $argon2id$") {
		t.Fatalf("config.yaml:\n%s", cfg)
	}
	if !strings.Contains(cfg, "# 管理员密码") {
		t.Error("template comments lost")
	}
	if ok, _ := password.Verify(m.Snapshot().Config.Admin.PasswordHash, testPassword); !ok {
		t.Fatal("stored hash does not verify")
	}
	// Second start without env works and ignores nothing.
	if _, err := Open(dir, Options{}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, FileConfig)); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("config.yaml mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestPlaintextPasswordIsHashedInPlace(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, FileConfig, "admin:\n  username: root\n  # my password\n  password: "+testPassword+"   # set by hand\nlogs:\n  retention_days: 7\n")
	m, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, dir, FileConfig)
	if strings.Contains(got, testPassword) {
		t.Fatalf("plaintext left on disk:\n%s", got)
	}
	if !strings.Contains(got, "  # my password\n  password_hash: $argon2id$") || !strings.Contains(got, "   # set by hand\nlogs:") {
		t.Errorf("comments not preserved:\n%s", got)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "backups"))
	if len(entries) != 0 {
		t.Errorf("plaintext rewrite must not create backups (they would hold the password): %v", entries)
	}
	if m.Snapshot().Config.Admin.Username != "root" || m.Snapshot().Config.Logs.RetentionDays != 7 {
		t.Errorf("config = %+v", m.Snapshot().Config)
	}
}

func TestStartupErrorsHaveLines(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, FileConfig, "admin:\n  password: "+testPassword+"\nlogs:\n  retension_days: 7\n")
	write(t, dir, KeysFile("tavily"), "keys:\n  - key: tvly-a\n    egress: nowhere\n")
	_, err := Open(dir, Options{})
	errs, ok := err.(Errors)
	if !ok || len(errs) != 1 {
		t.Fatalf("err = %v", err)
	}
	e := errs[0]
	if e.File != FileConfig || e.Line != 4 || !strings.Contains(e.Msg, `did you mean "retention_days"`) {
		t.Errorf("error = %+v", e)
	}
	// Fix config: now the dangling egress reference is reported with its line.
	write(t, dir, FileConfig, "admin:\n  password: "+testPassword+"\n")
	_, err = Open(dir, Options{})
	errs, _ = err.(Errors)
	if len(errs) != 1 || errs[0].File != KeysFile("tavily") || errs[0].Line != 3 {
		t.Fatalf("err = %v", err)
	}
}

func TestReloadAppliesHandEdits(t *testing.T) {
	m, dir := openTest(t)
	rec := &recorder{}
	m.Subscribe(rec.fn)
	write(t, dir, KeysFile("tavily"), "keys:\n  - key: tvly-abc-0001\n    label: free-01\n  - tvly-abc-0002\n")
	m.Reload()
	keys := m.Snapshot().KeysFor("tavily")
	if len(keys) != 2 || keys[0].Label != "free-01" || !keys[1].Egress.Direct() || keys[1].Plan != "free" {
		t.Fatalf("keys = %+v", keys)
	}
	c, n := rec.last()
	if n != 1 || c.Source != SourceFile || len(c.Files) != 1 || !strings.Contains(strings.Join(c.Summary, "\n"), "+key tavily free-01") {
		t.Fatalf("change = %+v", c)
	}
	if strings.Contains(strings.Join(c.Summary, "\n"), "tvly-abc-0002") {
		t.Error("summary leaks a key")
	}
	// No-op reload does not notify.
	m.Reload()
	if _, n2 := rec.last(); n2 != 1 {
		t.Error("spurious notification")
	}
}

func TestBrokenEditKeepsLastGood(t *testing.T) {
	m, dir := openTest(t)
	write(t, dir, KeysFile("exa"), "keys:\n  - exa-good-key\n")
	m.Reload()
	before := m.Snapshot()

	write(t, dir, KeysFile("exa"), "keys:\n  - exa-good-key\n  - key: [broken\n")
	m.Reload()
	if m.Snapshot() != before {
		t.Fatal("broken file replaced the snapshot")
	}
	st := statusOf(m, KeysFile("exa"))
	if st.Applied || len(st.Errors) == 0 || st.Errors[0].Line == 0 {
		t.Fatalf("status = %+v", st)
	}
	if m.Healthy() {
		t.Error("Healthy() should be false")
	}
	write(t, dir, KeysFile("exa"), "keys:\n  - exa-good-key\n  - exa-second-key\n")
	m.Reload()
	if len(m.Snapshot().KeysFor("exa")) != 2 || !statusOf(m, KeysFile("exa")).Applied || !m.Healthy() {
		t.Fatalf("fix not applied: %+v", statusOf(m, KeysFile("exa")))
	}
}

func TestCrossFileRenameRequiresBothFiles(t *testing.T) {
	m, dir := openTest(t)
	write(t, dir, FileEgresses, "egresses:\n  - name: hk-1\n    url: socks5://1.1.1.1:1080\n")
	write(t, dir, KeysFile("tavily"), "keys:\n  - key: tvly-k1\n    egress: hk-1\n")
	m.Reload()
	if m.Snapshot().Keys[0].Egress.Name != "hk-1" {
		t.Fatalf("setup: %+v", m.Snapshot().Keys)
	}
	// Rename the node only: rejected because keys/tavily.yaml still says hk-1.
	write(t, dir, FileEgresses, "egresses:\n  - name: hk-9\n    url: socks5://1.1.1.1:1080\n")
	m.Reload()
	st := statusOf(m, FileEgresses)
	if st.Applied || len(st.Errors) == 0 || !strings.Contains(st.Errors[0].Msg, "breaks a reference") {
		t.Fatalf("egresses status = %+v", st)
	}
	if _, ok := m.Snapshot().Egresses.Lookup("hk-1"); !ok {
		t.Fatal("old node should still be in effect")
	}
	// Update the reference too: both apply together.
	write(t, dir, KeysFile("tavily"), "keys:\n  - key: tvly-k1\n    egress: hk-9\n")
	m.Reload()
	if m.Snapshot().Keys[0].Egress.Name != "hk-9" || !m.Healthy() {
		t.Fatalf("after fix: %+v %+v", m.Snapshot().Keys, m.Status())
	}
}

func TestNewKeyFileErrorDoesNotBlockValidEgressChange(t *testing.T) {
	m, dir := openTest(t)
	write(t, dir, FileEgresses, "egresses:\n  - name: us-1\n    url: http://2.2.2.2:3128\n")
	write(t, dir, KeysFile("tavily"), "keys:\n  - key: tvly-x\n    egress: typo-node\n")
	m.Reload()
	if _, ok := m.Snapshot().Egresses.Lookup("us-1"); !ok {
		t.Error("valid egresses.yaml should apply")
	}
	if statusOf(m, KeysFile("tavily")).Applied {
		t.Error("broken keys file should be rejected")
	}
}

func TestMissingFileAtRuntime(t *testing.T) {
	m, dir := openTest(t)
	before := m.Snapshot()
	if err := os.Remove(filepath.Join(dir, FileTokens)); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	if m.Snapshot() != before || statusOf(m, FileTokens).Applied {
		t.Fatal("missing file should keep the last applied version and report it")
	}
}

func TestTokenPlaintextHashed(t *testing.T) {
	m, dir := openTest(t)
	plain := "team-shared-token-0123456789"
	write(t, dir, FileTokens, "tokens:\n  # the laptop\n  - name: laptop\n    token: "+plain+"\n    limits:\n      rpm: 60\n")
	m.Reload()
	got := read(t, dir, FileTokens)
	if strings.Contains(got, plain) || !strings.Contains(got, "token_hash: "+token.Hash(plain)) || !strings.Contains(got, "token_prefix: team-s") {
		t.Fatalf("tokens.yaml:\n%s", got)
	}
	tk, ok := m.Snapshot().TokenByHash(token.Hash(plain))
	if !ok || tk.Name != "laptop" || tk.Limits.RPM != 60 || len(tk.Providers) != 2 || tk.KeyGroup != "all" {
		t.Fatalf("token = %+v", tk)
	}
}

func TestEditConflictsAndBackups(t *testing.T) {
	m, dir := openTest(t)
	m.opts.BackupKeep = 2
	v := m.Snapshot().Versions[KeysFile("tavily")]

	// Stale base version → conflict.
	_, err := m.Edit(SourcePanel, map[string]string{KeysFile("tavily"): "deadbeef"}, func(tx *Tx) error {
		_, err := tx.Doc(KeysFile("tavily"))
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}

	// Unapplied (broken) hand edit on disk → conflict, and it is not overwritten.
	write(t, dir, KeysFile("tavily"), "keys: [oops\n")
	m.Reload()
	_, _, err = m.AddKeys(nil, []KeyInput{{Secret: "tvly-new"}})
	if !errors.Is(err, ErrConflict) || read(t, dir, KeysFile("tavily")) != "keys: [oops\n" {
		t.Fatalf("hand edit must not be overwritten: %v", err)
	}

	// Restore and make several edits: backups are pruned to BackupKeep.
	tpl := "keys:\n  # keep me\n  - tvly-a\n"
	write(t, dir, KeysFile("tavily"), tpl)
	m.Reload()
	for i, s := range []string{"tvly-b", "tvly-c", "tvly-d"} {
		if _, _, err := m.AddKeys(nil, []KeyInput{{Secret: s, Label: "k" + string(rune('0'+i))}}); err != nil {
			t.Fatal(err)
		}
	}
	got := read(t, dir, KeysFile("tavily"))
	if !strings.HasPrefix(got, tpl) || !strings.Contains(got, "  - key: tvly-d\n    label: k2\n") {
		t.Fatalf("surgical append lost formatting:\n%s", got)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "backups"))
	if len(entries) != 2 {
		t.Errorf("backups = %d, want 2", len(entries))
	}
	_ = v
}

func TestSaveRawValidatesAndHashes(t *testing.T) {
	m, dir := openTest(t)
	raw, v, err := m.ReadRaw(FileConfig)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), "retention_days: 30", "retention_days: 0", 1)
	if _, err := m.SaveRaw(FileConfig, v, []byte(bad)); err == nil {
		t.Fatal("invalid raw content accepted")
	} else if errs, ok := err.(Errors); !ok || errs[0].Line == 0 {
		t.Fatalf("want positioned errors, got %v", err)
	}
	if read(t, dir, FileConfig) != string(raw) {
		t.Fatal("invalid content was written")
	}
	// Typing a new plaintext password in the raw editor gets hashed before writing.
	newPw := "another-long-password"
	good := strings.Replace(string(raw), "retention_days: 30", "retention_days: 14", 1)
	good = strings.Replace(good, "  username: admin\n", "  username: admin\n  password: "+newPw+"\n", 1)
	if _, err := m.SaveRaw(FileConfig, v, []byte(good)); err != nil {
		t.Fatal(err)
	}
	disk := read(t, dir, FileConfig)
	if strings.Contains(disk, newPw) || strings.Count(disk, "password_hash:") != 1 {
		t.Fatalf("config.yaml:\n%s", disk)
	}
	if ok, _ := password.Verify(m.Snapshot().Config.Admin.PasswordHash, newPw); !ok || m.Snapshot().Config.Logs.RetentionDays != 14 {
		t.Fatal("raw save not applied")
	}
}

func TestKeyOpsAndEgressCompatibility(t *testing.T) {
	m, dir := openTest(t)
	if _, _, err := m.ImportEgresses(nil, "1.1.1.1:1080:u:p #hk-1\n2.2.2.2:1080", egress.ImportOptions{NamePrefix: "node-"}, "HK", []string{"res"}, false); err != nil {
		t.Fatal(err)
	}
	items, perrs := ParseKeyLines("tvly-key-0001,free-01,hk-1\n# comment\nexa-key-0001,exa-01,socks5://u:p@1.1.1.1:1080,paid\ntvly-key-0002,,9.9.9.9:1080\n")
	if len(perrs) != 0 || len(items) != 3 {
		t.Fatalf("ParseKeyLines: %v %v", items, perrs)
	}
	added, skipped, err := m.AddKeys(nil, items)
	if err != nil || len(added) != 3 || len(skipped) != 0 {
		t.Fatalf("AddKeys: %v %v %v", added, skipped, err)
	}
	s := m.Snapshot()
	tav, exa := s.KeysFor("tavily"), s.KeysFor("exa")
	if len(tav) != 2 || len(exa) != 1 {
		t.Fatalf("provider routing: %d/%d", len(tav), len(exa))
	}
	// URL matching a node was written as the node name (§7.9.2 rule 3).
	if exa[0].EgressRef != "hk-1" || exa[0].Plan != "paid" || exa[0].Priority != 1 {
		t.Fatalf("exa key = %+v", exa[0])
	}
	// Unknown URL stays inline with an explicit port, as an unsaved egress.
	if tav[1].EgressRef != "socks5://9.9.9.9:1080" || tav[1].Egress.Name != "" || !tav[1].Egress.Inline {
		t.Fatalf("inline key = %+v", tav[1])
	}
	// Duplicate add is skipped.
	if _, sk, _ := m.AddKeys(nil, []KeyInput{{Secret: "tvly-key-0001"}}); len(sk) != 1 {
		t.Error("duplicate not skipped")
	}

	// Import the inline proxy as a node and absorb the key's inline reference.
	rows, _, err := m.ImportEgresses(nil, "9.9.9.9:1080 #sg-1", egress.ImportOptions{}, "", nil, true)
	if err != nil || len(rows) != 1 || rows[0].Status != egress.ImportAbsorbs {
		t.Fatalf("import absorb: %+v %v", rows, err)
	}
	if k, _ := m.Snapshot().KeyByID(tav[1].ID); k.EgressRef != "sg-1" {
		t.Fatalf("inline ref not absorbed: %+v", k)
	}

	// Rename cascades to key files, including defaults.
	if _, err := m.SetConfig(nil, "egress.max_keys_per_egress", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateKeys(nil, []string{tav[0].ID}, KeyPatch{Egress: ptr("1.1.1.1:1080:u:p")}); err != nil {
		t.Fatal(err)
	}
	if k, _ := m.Snapshot().KeyByID(tav[0].ID); k.EgressRef != "hk-1" {
		t.Fatalf("patch egress by URL should canonicalize to node name: %+v", k)
	}
	kfile := read(t, dir, KeysFile("tavily"))
	kfile = strings.Replace(kfile, "  egress: direct", "  egress: hk-1", 1) // defaults use hk-1 too
	write(t, dir, KeysFile("tavily"), kfile)
	m.Reload()
	if _, err := m.RenameEgress(nil, "hk-1", "hk-main"); err != nil {
		t.Fatal(err)
	}
	s = m.Snapshot()
	for _, k := range s.Keys {
		if strings.EqualFold(k.EgressRef, "hk-1") {
			t.Fatalf("reference not renamed: %+v", k)
		}
	}
	if !strings.Contains(read(t, dir, KeysFile("tavily")), "  egress: hk-main") {
		t.Fatal("defaults.egress not renamed")
	}

	// Delete in use: refused, then moved to another node.
	if _, err := m.DeleteEgress(nil, "hk-main", DeleteRefuse, ""); err == nil {
		t.Fatal("delete of in-use node should be refused")
	} else {
		var iu *InUseError
		if !errors.As(err, &iu) || len(iu.Keys) < 2 {
			t.Fatalf("want InUseError, got %v", err)
		}
	}
	if _, err := m.DeleteEgress(nil, "hk-main", DeleteToNode, "node-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Snapshot().Egresses.Lookup("hk-main"); ok {
		t.Fatal("node not deleted")
	}
	for _, k := range m.Snapshot().Keys {
		if k.Egress.Name == "hk-main" {
			t.Fatalf("key still on deleted node: %+v", k)
		}
	}

	// Delete keys.
	if _, err := m.DeleteKeys(nil, []string{tav[0].ID, exa[0].ID}); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot().Keys) != 1 {
		t.Fatalf("keys left = %d", len(m.Snapshot().Keys))
	}
}

func TestSetConfigCreatesSectionsAndValidates(t *testing.T) {
	m, dir := openTest(t)
	// Remove the whole logs section by hand; SetConfig must recreate it surgically.
	cfg := read(t, dir, FileConfig)
	start := strings.Index(cfg, "logs:\n")
	end := strings.Index(cfg, "network:\n")
	write(t, dir, FileConfig, cfg[:start]+cfg[end:])
	m.Reload()
	if !statusOf(m, FileConfig).Applied {
		t.Fatalf("setup: %+v", statusOf(m, FileConfig))
	}
	if _, err := m.SetConfig(nil, "logs.retention_days", 0); err == nil {
		t.Fatal("out-of-range value accepted")
	}
	if _, err := m.SetConfig(nil, "logs.retention_days", 14); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(read(t, dir, FileConfig), "health_interval_seconds: 60   # 出口健康检查间隔（秒）\nlogs:\n  retention_days: 14\n") {
		t.Errorf("section not appended surgically:\n%s", read(t, dir, FileConfig))
	}
	if _, err := m.SetConfig(nil, "routing.max_attempts", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetConfig(nil, "upstream.extra_headers", []string{"X-Custom"}); err != nil {
		t.Fatal(err)
	}
	c := m.Snapshot().Config
	if c.Routing.MaxAttempts != 5 || len(c.Upstream.ExtraHeaders) != 1 || c.Logs.RetentionDays != 14 {
		t.Fatalf("config = %+v", c)
	}
	if !strings.Contains(read(t, dir, FileConfig), "max_attempts: 5               # 单个请求最多尝试几个 key") {
		t.Errorf("inline comment not preserved:\n%s", read(t, dir, FileConfig))
	}
	if _, err := m.SetConfig(nil, "admin.password_hash", "x"); err == nil {
		t.Error("protected path accepted")
	}
}

func TestPasswordAndTOTPOps(t *testing.T) {
	m, _ := openTest(t)
	rec := &recorder{}
	m.Subscribe(rec.fn)
	if _, err := m.SetAdminPassword("short"); err == nil {
		t.Fatal("weak password accepted")
	}
	if _, err := m.SetAdminPassword("brand-new-password"); err != nil {
		t.Fatal(err)
	}
	c, _ := rec.last()
	if c.Old.Config.Admin.PasswordHash == c.New.Config.Admin.PasswordHash || !strings.Contains(strings.Join(c.Summary, ";"), "admin password changed") {
		t.Fatalf("change = %+v", c.Summary)
	}
	if _, err := m.SetTOTPSecret("JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Config.Admin.TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Fatal("totp not set")
	}
	if _, err := m.SetTOTPSecret(""); err != nil || m.Snapshot().Config.Admin.TOTPSecret != "" {
		t.Fatalf("totp not removed: %v", err)
	}
}

func TestWatchPicksUpEdits(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir, Options{AdminPasswordEnv: testPassword, PollInterval: 50 * time.Millisecond, Debounce: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := contextWithCancel()
	defer cancel()
	go m.Watch(ctx)
	write(t, dir, KeysFile("exa"), "keys:\n  - exa-watched-key\n")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.Snapshot().KeysFor("exa")) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watcher did not pick up the edit")
}

func TestMaskSecretAndKeyID(t *testing.T) {
	if got := MaskSecret("tvly-dev-abcdefghij1234"); got != "tvly-dev-…1234" {
		t.Errorf("MaskSecret = %q", got)
	}
	if MaskSecret("short") != "*****" {
		t.Error("short secrets must be fully masked")
	}
	if KeyID("tavily", "x") == KeyID("exa", "x") || len(KeyID("tavily", "x")) != 12 {
		t.Error("KeyID")
	}
}

func ptr[T any](v T) *T { return &v }
