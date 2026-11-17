package confstore

import (
	"strings"
	"testing"
)

func TestKeyTeamAndRateLimit(t *testing.T) {
	m, dir := openTest(t)
	write(t, dir, KeysFile("exa"), "defaults:\n  team: main\nkeys:\n  - exa-aaaa-1\n  - key: exa-bbbb-2\n    team: other\n    rate_limit: 25\n")
	m.Reload()
	keys := m.Snapshot().KeysFor("exa")
	if len(keys) != 2 || keys[0].Team != "main" || keys[0].RateLimit != 0 || keys[1].Team != "other" || keys[1].RateLimit != 25 {
		t.Fatalf("keys = %+v", keys)
	}

	write(t, dir, KeysFile("tavily"), "keys:\n  - key: tvly-a\n    team: nope\n")
	m.Reload()
	st := statusOf(m, KeysFile("tavily"))
	if st.Applied || len(st.Errors) != 1 || st.Errors[0].Line != 3 || !strings.Contains(st.Errors[0].Msg, "Exa keys only") {
		t.Fatalf("status = %+v", st)
	}
	write(t, dir, KeysFile("tavily"), "keys:\n  - key: tvly-a\n    rate_limit: 1000\n")
	m.Reload()
	if k := m.Snapshot().KeysFor("tavily"); len(k) != 1 || k[0].RateLimit != 1000 {
		t.Fatalf("tavily keys = %+v", k)
	}

	// A shorthand entry gets expanded, and the file's default team keeps applying.
	if _, err := m.UpdateKeys(nil, []string{keys[0].ID}, KeyPatch{Team: ptr(""), RateLimit: ptr(25.0)}); err != nil {
		t.Fatal(err)
	}
	if k, _ := m.Snapshot().KeyByID(keys[0].ID); k.RateLimit != 25 || k.Team != "main" {
		t.Fatalf("patched key = %+v", k)
	}
}
