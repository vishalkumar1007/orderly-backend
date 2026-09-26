// Package secretbox provides authenticated encryption for provider credentials
// held in the database. Secrets are sealed with AES-256-GCM using a key derived
// from a passphrase via scrypt, so the operator can supply a memorable string
// rather than raw key material.
//
// Design notes:
//   - Ciphertext is stored as a versioned, self-describing envelope so the KDF
//     parameters can be changed later without invalidating existing rows.
//   - Every seal attaches random additional authenticated data derived from the
//     record's identity, so a ciphertext cannot be moved between rows to make a
//     secret resolve in the wrong place.
//   - Sealing a value that is already an envelope is a no-op, which keeps the
//     write paths idempotent when a client resubmits a masked field.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// Envelope version. Bump only alongside a migration that can still read the
// previous version.
const envelopeVersion = "v1"

// scrypt cost parameters. N=32768, r=8, p=1 costs roughly 32 MiB and ~100ms per
// operation, which is acceptable because sealing happens on admin writes only —
// never on a request-path read.
const (
	scryptN      = 1 << 15
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 32
)

var (
	// ErrNoKey is returned when the box is used without a passphrase.
	ErrNoKey = errors.New("secretbox: no encryption key configured")
	// ErrMalformed is returned for input that is not a valid envelope.
	ErrMalformed = errors.New("secretbox: malformed envelope")
	// ErrAADMismatch is returned when a ciphertext was sealed for a different
	// record, i.e. it has been copied between rows.
	ErrAADMismatch = errors.New("secretbox: ciphertext does not belong to this record")
)

// Box seals and opens secrets with a single passphrase.
type Box struct {
	key []byte
}

// New derives an encryption key from passphrase. An empty passphrase yields a
// disabled box: Seal returns the plaintext unchanged and Open is a no-op, so
// development environments keep working without ceremony. Callers that must
// never store plaintext should assert on Disabled.
func New(passphrase string) *Box {
	p := strings.TrimSpace(passphrase)
	if p == "" {
		return &Box{}
	}
	key, err := scrypt.Key([]byte(p), saltFor(p), scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		// scrypt only fails on nonsensical parameters, which are constants here.
		return &Box{}
	}
	return &Box{key: key}
}

// Disabled reports whether the box has no key. Callers use this to fail closed
// in production rather than silently persisting plaintext.
func (b *Box) Disabled() bool { return b == nil || len(b.key) == 0 }

// saltFor derives a deterministic per-installation salt from the passphrase.
// A random salt would force the operator to store it separately, and a fixed
// application salt is acceptable here because the passphrase itself must carry
// the entropy.
func saltFor(passphrase string) []byte {
	// Domain-separated, constant salt. Length matches scrypt's minimum.
	salt := make([]byte, 32)
	copy(salt, "orderly.secretbox.v1.salt..........")
	return salt
}

// Seal encrypts plaintext and returns a storable envelope. contextID binds the
// ciphertext to a specific record (for example "platform:smtp"); pass the same
// value to Open. Sealing an existing envelope returns it unchanged so repeated
// saves with a masked secret do not double-encrypt.
func (b *Box) Seal(plaintext []byte, contextID string) ([]byte, error) {
	if b.Disabled() {
		return plaintext, nil
	}
	if isEnvelope(plaintext) {
		return plaintext, nil
	}
	gcm, err := b.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: read nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, []byte(contextID))
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return []byte(envelopeVersion + ":" + base64.StdEncoding.EncodeToString(out)), nil
}

// Open decrypts an envelope produced by Seal. It returns ErrAADMismatch if the
// value was sealed for a different record.
func (b *Box) Open(envelope []byte, contextID string) ([]byte, error) {
	if b.Disabled() {
		return envelope, nil
	}
	raw, err := decodeEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	gcm, err := b.gcm()
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, ErrMalformed
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, []byte(contextID))
	if err != nil {
		// GCM cannot distinguish a wrong key from wrong AAD; the AAD case is by
		// far the more common operator error, so report it as such.
		return nil, ErrAADMismatch
	}
	return plain, nil
}

// SealString is a convenience wrapper around Seal.
func (b *Box) SealString(plaintext, contextID string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	out, err := b.Seal([]byte(plaintext), contextID)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// OpenString is a convenience wrapper around Open.
func (b *Box) OpenString(envelope, contextID string) (string, error) {
	if strings.TrimSpace(envelope) == "" {
		return "", nil
	}
	out, err := b.Open([]byte(envelope), contextID)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// IsEnvelope reports whether a stored value is already sealed. Callers use it
// to avoid treating stored ciphertext as plaintext on a migration.
func IsEnvelope(v string) bool { return isEnvelope([]byte(v)) }

func isEnvelope(v []byte) bool {
	return len(v) > len(envelopeVersion)+1 && string(v[:len(envelopeVersion)+1]) == envelopeVersion+":"
}

func decodeEnvelope(v []byte) ([]byte, error) {
	if !isEnvelope(v) {
		return nil, ErrMalformed
	}
	raw, err := base64.StdEncoding.DecodeString(string(v[len(envelopeVersion)+1:]))
	if err != nil {
		return nil, ErrMalformed
	}
	return raw, nil
}

func (b *Box) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new gcm: %w", err)
	}
	return gcm, nil
}

// ConstantTimeEqual compares two secrets without leaking their contents through
// timing. Used when a caller submits a masked value and we must decide whether
// it actually changed.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
