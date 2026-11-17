package confstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Found in acceptance (2026-11-10): a config file replaced by a root-owned copy
// (scp, sudo cp) was reported as "missing", with advice that did not help.
func TestUnreadableFileAtRuntime(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs file modes and a non-root user")
	}
	m, dir := openTest(t)
	before := m.Snapshot()
	p := filepath.Join(dir, FileTokens)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	m.Reload()
	st := statusOf(m, FileTokens)
	if m.Snapshot() != before || st.Applied || len(st.Errors) != 1 {
		t.Fatalf("an unreadable file must keep the applied version and be reported: %+v", st)
	}
	if msg := st.Errors[0].Msg; !strings.Contains(msg, "cannot be read") || !strings.Contains(msg, "permission denied") || strings.Contains(msg, "missing") {
		t.Fatalf("message = %q", msg)
	}
	if m.Healthy() {
		t.Fatal("health must be degraded")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	if st := statusOf(m, FileTokens); !st.Applied || len(st.Errors) != 0 || !m.Healthy() {
		t.Fatalf("not recovered: %+v", st)
	}
}
