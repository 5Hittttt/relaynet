// Package crypto provides key derivation, authentication MAC and AEAD helpers.
package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// AuthToken returns SHA-256(password), used as the room credential presented to the server.
// The server stores and compares this token; the plaintext password never leaves the client.
func AuthToken(password string) []byte {
	sum := sha256.Sum256([]byte(password))
	return sum[:]
}

// DeriveDataKey derives a 32-byte ChaCha20-Poly1305 key from the password and room ID.
// All room members derive the same key, enabling end-to-end encryption of game traffic.
// The relay server never needs (and never uses) this key.
func DeriveDataKey(password, roomID string) []byte {
	prk := sha256.Sum256([]byte("relaynet:" + password))
	kdf := hkdf.New(sha256.New, prk[:], []byte("relaynet-data:"+roomID), []byte("v1"))
	key := make([]byte, chacha20poly1305.KeySize)
	io.ReadFull(kdf, key)
	return key
}

// Seal encrypts plaintext with key and returns nonce||ciphertext.
func Seal(key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plaintext, nil)
	return append(nonce, ct...), nil
}

// Open decrypts a nonce||ciphertext blob produced by Seal.
func Open(key, blob []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(blob) < ns+aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, blob[:ns], blob[ns:], nil)
}

// VerifyToken returns true if token equals SHA-256(password).
func VerifyToken(password string, token []byte) bool {
	expected := AuthToken(password)
	return hmac.Equal(expected, token)
}

// EqualToken compares two auth tokens in constant time.
func EqualToken(a, b []byte) bool {
	return hmac.Equal(a, b)
}
