// Package secrets encrypts values before they are stored, so a database dump
// or backup does not expose them.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const keySize = 32

// Box seals and opens values with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// ParseKey reads a base64 encoded 32 byte key (openssl rand -base64 32).
func ParseKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secrets key is not valid base64: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("secrets key must be %d bytes, got %d", keySize, len(key))
	}
	return key, nil
}

// New returns a Box for a 32 byte key.
func New(key []byte) (*Box, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("secrets key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts value with a fresh random nonce, which is stored in front of
// the ciphertext. Sealing the same value twice gives different bytes.
func (b *Box) Seal(value string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(value), nil), nil
}

// Open decrypts what Seal returned. It fails when the data was altered or
// sealed with another key.
func (b *Box) Open(sealed []byte) (string, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return "", errors.New("secrets: sealed value is too short")
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", errors.New("secrets: cannot open sealed value")
	}
	return string(plain), nil
}
