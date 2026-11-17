package password

import (
	"strings"
	"testing"
)

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") || !IsHash(h) {
		t.Fatalf("bad PHC: %s", h)
	}
	if ok, err := Verify(h, "correct horse battery"); err != nil || !ok {
		t.Fatalf("verify good: %v %v", ok, err)
	}
	if ok, _ := Verify(h, "correct horse batterY"); ok {
		t.Fatal("verify accepted wrong password")
	}
	h2, _ := Hash("correct horse battery")
	if h == h2 {
		t.Fatal("salt must be random")
	}
}

func TestPolicy(t *testing.T) {
	if _, err := Hash("short"); err == nil {
		t.Error("short password accepted")
	}
	if err := Validate("十二个汉字的密码长度足够了吗"); err != nil { // counted in characters, not bytes
		t.Errorf("CJK password rejected: %v", err)
	}
}

func TestRejectsMalformedAndAbusiveParams(t *testing.T) {
	good, _ := Hash("correct horse battery")
	parts := strings.Split(good, "$")
	bad := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$" + parts[4] + "$" + parts[5],
		"$argon2id$v=16$m=65536,t=3,p=2$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=4194304,t=3,p=2$" + parts[4] + "$" + parts[5], // 4 GiB memory bomb
		"$argon2id$v=19$m=65536,t=999,p=2$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=2$!!$" + parts[5],
	}
	for _, s := range bad {
		if IsHash(s) {
			t.Errorf("accepted %q", s)
		}
		if _, err := Verify(s, "x"); err == nil {
			t.Errorf("Verify accepted %q", s)
		}
	}
}
