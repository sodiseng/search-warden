package confstore

import (
	"strings"
	"testing"

	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
)

func TestTokenOps(t *testing.T) {
	m, dir := openTest(t)
	spec := TokenSpec{Name: "laptop", Providers: []string{"tavily"}, Surfaces: []string{"mcp"}, KeyGroup: "free",
		IPAllow: []string{"10.0.0.0/8"}, Expires: "2027-01-01", Enabled: true, Limits: TokenLimits{RPM: 60, MonthlyUSD: 2.5}, Note: "n"}
	plain, snap, err := m.AddToken(nil, spec)
	if err != nil {
		t.Fatal(err)
	}
	if token.Validate(plain) != nil || strings.Contains(read(t, dir, FileTokens), plain) {
		t.Fatal("plaintext token stored or invalid")
	}
	tk, ok := snap.TokenByHash(token.Hash(plain))
	if !ok || tk.Name != "laptop" || tk.KeyGroup != "free" || tk.Limits.RPM != 60 || tk.Limits.MonthlyUSD != 2.5 || len(tk.IPAllow) != 1 ||
		tk.Expires.Year() != 2027 || len(tk.Providers) != 1 || len(tk.Surfaces) != 1 {
		t.Fatalf("token = %+v", tk)
	}
	if got := SpecOf(tk); got.Name != "laptop" || len(got.Providers) != 1 || got.Expires == "" || got.KeyGroup != "free" {
		t.Fatalf("spec = %+v", got)
	}

	// Edit: widen the scope, drop limits; the secret stays.
	spec.Providers, spec.Surfaces, spec.Limits, spec.KeyGroup, spec.Note = nil, nil, TokenLimits{}, "all", ""
	snap, err = m.UpdateToken(nil, tk.ID, spec)
	if err != nil {
		t.Fatal(err)
	}
	tk, _ = snap.TokenByHash(token.Hash(plain))
	if len(tk.Providers) != 2 || tk.Limits.RPM != 0 || tk.KeyGroup != "all" {
		t.Fatalf("updated = %+v", tk)
	}
	f := read(t, dir, FileTokens)
	var entry string
	for _, line := range strings.Split(f[strings.Index(f, "name: laptop"):], "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			entry += line + "\n"
		}
	}
	if strings.Contains(entry, "limits") || strings.Contains(entry, "providers") || strings.Contains(entry, "key_group") {
		t.Fatalf("defaults written to the entry:\n%s", entry)
	}
	if _, err := m.SetTokenEnabled(nil, tk.ID, false); err != nil {
		t.Fatal(err)
	}
	if tk, _ = m.Snapshot().TokenByHash(token.Hash(plain)); tk.Enabled {
		t.Fatal("still enabled")
	}

	// Rotate: the old secret stops working.
	newPlain, snap, err := m.RotateToken(nil, tk.ID)
	if err != nil || newPlain == plain {
		t.Fatal(err)
	}
	if _, ok := snap.TokenByHash(token.Hash(plain)); ok {
		t.Fatal("old secret still valid")
	}
	rotated, ok := snap.TokenByHash(token.Hash(newPlain))
	if !ok || rotated.Name != "laptop" {
		t.Fatal("rotated token missing")
	}

	// Names are unique; a bad edit is rejected as a whole.
	if _, _, err := m.AddToken(nil, TokenSpec{Name: "Laptop", Enabled: true}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := m.UpdateToken(nil, rotated.ID, TokenSpec{Name: "laptop", Expires: "soon", Enabled: true}); err == nil {
		t.Fatal("bad expiry accepted")
	}

	if _, err := m.DeleteToken(nil, rotated.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot().Tokens) != 0 {
		t.Fatal("token not deleted")
	}
	if l := ParseList(" a, b\nc,,"); strings.Join(l, "|") != "a|b|c" {
		t.Fatal(l)
	}
}
