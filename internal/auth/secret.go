package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// NewSecret returns 32 random bytes, URL-safe base64 encoded. Used for
// session cookies and API tokens.
func NewSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashSecret is how secrets are stored and looked up. They're random, so
// a plain SHA-256 is enough (no salt or slow hash needed).
func HashSecret(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// passwordLetters leaves out letters that look alike (0/o, 1/l/i).
const passwordLetters = "abcdefghjkmnpqrstuvwxyz23456789"

// NewPassword returns a random password that's easy to type: five groups
// of four letters and digits joined by dashes, about 99 bits.
func NewPassword() string {
	b := make([]byte, 20)
	rand.Read(b)
	var out []byte
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		// 256 isn't a multiple of 31, so the first few letters are
		// slightly likelier. It costs a fraction of a bit, which 99 bits
		// can spare.
		out = append(out, passwordLetters[int(c)%len(passwordLetters)])
	}
	return string(out)
}
