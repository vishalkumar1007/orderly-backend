package secretbox

import (
	"bytes"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b := New("correct horse battery staple")
	if b.Disabled() {
		t.Fatal("box should be enabled with a passphrase")
	}
	secret := []byte("super-secret-password")

	env, err := b.Seal(secret, "platform:smtp")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(env, secret) {
		t.Fatal("envelope must not contain the plaintext secret")
	}
	if !IsEnvelope(string(env)) {
		t.Fatalf("sealed value is not recognisable as an envelope: %q", env)
	}

	got, err := b.Open(env, "platform:smtp")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round trip mismatch: got %q want %q", got, secret)
	}
}

func TestSealIsIdempotent(t *testing.T) {
	b := New("pass")
	first, err := b.Seal([]byte("value"), "tenant:x:ai")
	if err != nil {
		t.Fatal(err)
	}
	// A masked resubmit passes the stored envelope straight back through.
	second, err := b.Seal(first, "tenant:x:ai")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("re-sealing an envelope must not double-encrypt")
	}
}

func TestOpenRejectsWrongRecord(t *testing.T) {
	b := New("pass")
	env, err := b.Seal([]byte("tenant-secret"), "tenant:abc:storage")
	if err != nil {
		t.Fatal(err)
	}
	// Same ciphertext, different record identity: must not decrypt.
	if _, err := b.Open(env, "tenant:xyz:storage"); err != ErrAADMismatch {
		t.Fatalf("want ErrAADMismatch, got %v", err)
	}
	if _, err := b.Open(env, "platform:smtp"); err != ErrAADMismatch {
		t.Fatalf("want ErrAADMismatch, got %v", err)
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	env, err := New("key-one").Seal([]byte("secret"), "platform:smtp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New("key-two").Open(env, "platform:smtp"); err != ErrAADMismatch {
		t.Fatalf("want ErrAADMismatch for wrong key, got %v", err)
	}
}

func TestOpenRejectsMalformed(t *testing.T) {
	b := New("pass")
	for _, in := range []string{"", "plaintext", "v2:abc", "v1:!!!not-base64!!!"} {
		if _, err := b.Open([]byte(in), "platform:smtp"); err == nil {
			t.Errorf("expected error for malformed input %q", in)
		}
	}
}

func TestDisabledBoxIsPassThrough(t *testing.T) {
	b := New("   ")
	if !b.Disabled() {
		t.Fatal("blank passphrase must yield a disabled box")
	}
	env, err := b.Seal([]byte("plain"), "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "plain" {
		t.Fatalf("disabled box must pass through unchanged, got %q", env)
	}
	got, err := b.Open(env, "ctx")
	if err != nil || string(got) != "plain" {
		t.Fatalf("disabled box open: got %q err %v", got, err)
	}
}

func TestNilBoxIsSafe(t *testing.T) {
	var b *Box
	if !b.Disabled() {
		t.Fatal("nil box must report disabled")
	}
	if _, err := b.Seal([]byte("x"), "c"); err != nil {
		t.Fatalf("nil box seal: %v", err)
	}
}

func TestEmptyStringHelpers(t *testing.T) {
	b := New("pass")
	s, err := b.SealString("", "ctx")
	if err != nil || s != "" {
		t.Fatalf("SealString(empty) = %q, %v", s, err)
	}
	got, err := b.OpenString("", "ctx")
	if err != nil || got != "" {
		t.Fatalf("OpenString(empty) = %q, %v", got, err)
	}
}

func TestNoncesAreUnique(t *testing.T) {
	b := New("pass")
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		env, err := b.Seal([]byte("same plaintext every time"), "ctx")
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(env)] {
			t.Fatal("identical plaintext produced an identical envelope (nonce reuse)")
		}
		seen[string(env)] = true
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc", "abc") {
		t.Error("equal strings should compare equal")
	}
	if ConstantTimeEqual("abc", "abd") {
		t.Error("different strings must not compare equal")
	}
	if !strings.EqualFold("ABC", "abc") {
		t.Error("sanity")
	}
}
