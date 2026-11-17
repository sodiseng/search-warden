package totp

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B test vector (SHA-1, secret "12345678901234567890"), truncated to 6 digits.
func TestRFC6238Vectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	cases := map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	}
	for unix, want := range cases {
		got, err := Code(secret, time.Unix(unix, 0))
		if err != nil || got != want {
			t.Errorf("t=%d: got %s, want %s (%v)", unix, got, want, err)
		}
	}
}

func TestVerifySkewAndReplay(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil || len(secret) != 32 {
		t.Fatalf("secret %q %v", secret, err)
	}
	now := time.Unix(1_800_000_000, 0)
	prev, _ := Code(secret, now.Add(-30*time.Second))
	cur, _ := Code(secret, now)
	old, _ := Code(secret, now.Add(-90*time.Second))

	c, ok := Verify(secret, prev, now, 0)
	if !ok {
		t.Fatal("previous step should be accepted")
	}
	if _, ok := Verify(secret, prev, now, c); ok {
		t.Fatal("replayed code accepted")
	}
	c2, ok := Verify(secret, " "+cur[:3]+" "+cur[3:]+" ", now, c) // spaces tolerated
	if !ok || c2 <= c {
		t.Fatal("current code rejected")
	}
	if _, ok := Verify(secret, old, now, 0); ok {
		t.Fatal("code three steps old accepted")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Verify(secret, bad, now, 0); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, ok := Verify("not base32!", cur, now, 0); ok {
		t.Error("bad secret accepted")
	}
}

func TestURI(t *testing.T) {
	u := URI("JBSWY3DPEHPK3PXP", "admin", "search-warden")
	if !strings.HasPrefix(u, "otpauth://totp/search-warden:admin?") || !strings.Contains(u, "secret=JBSWY3DPEHPK3PXP") {
		t.Errorf("URI = %s", u)
	}
}
