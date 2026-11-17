package token

import (
	"strings"
	"testing"
)

func TestGenerateValidateHash(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	// "sw_" + 32 lowercase letters or digits, nothing else.
	if !strings.HasPrefix(tok, Prefix) || len(tok) != len(Prefix)+bodyLen {
		t.Fatalf("format: %q", tok)
	}
	if !isLowerAlnum(tok[len(Prefix):]) {
		t.Fatalf("body must be lowercase alphanumeric: %q", tok)
	}
	if err := Validate(tok); err != nil {
		t.Fatalf("generated token invalid: %v", err)
	}
	other, _ := Generate()
	if tok == other {
		t.Fatal("tokens must be random")
	}
	h := Hash(tok)
	if !IsHash(h) || h == Hash(other) {
		t.Fatalf("hash: %s", h)
	}
	if DisplayPrefix(tok) != tok[:12] {
		t.Fatal("display prefix")
	}
}

func TestValidateRejects(t *testing.T) {
	tok, _ := Generate()
	body := tok[len(Prefix):]
	cases := map[string]string{
		"uppercase in the body": Prefix + strings.ToUpper(body),
		"too short":             Prefix + body[:20],
		"too long":              Prefix + body + "aaaa",
		"separator inside":      Prefix + body[:10] + "_" + body[10:],
		"short":                 "short",
		"space inside":          "has space in the middle ok",
		"tab inside":            "tab\tinside-long-enough-xxxx",
		"way too long":          strings.Repeat("a", 300),
	}
	for name, bad := range cases {
		if Validate(bad) == nil {
			t.Errorf("%s: accepted %q", name, bad)
		}
	}
	if err := Validate("my-own-team-token-0123456789"); err != nil {
		t.Errorf("custom token rejected: %v", err)
	}
}

func TestIsHash(t *testing.T) {
	if IsHash(strings.Repeat("A", 64)) || IsHash("abc") || !IsHash(strings.Repeat("0f", 32)) {
		t.Error("IsHash")
	}
}
