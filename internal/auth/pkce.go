package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// PKCE holds a verifier and its S256 challenge (RFC 7636).
type PKCE struct {
	Verifier  string
	Challenge string
}

func NewPKCE() (PKCE, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return PKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// RandomState returns an unguessable `state`.
func RandomState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
