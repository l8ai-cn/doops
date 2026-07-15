package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

func (s *GatewayStore) encryptSecret(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return s.encryptSecretEnvelope(plaintext, nil, "v1:")
}

func (s *GatewayStore) decryptSecret(ciphertext string) (string, error) {
	return s.decryptSecretEnvelope(ciphertext, nil, "v1:")
}

func (s *GatewayStore) encryptSecretWithAAD(plaintext string, aad []byte) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return s.encryptSecretEnvelope(plaintext, aad, "v2:")
}

func (s *GatewayStore) decryptSecretWithAAD(ciphertext string, aad []byte) (string, error) {
	return s.decryptSecretEnvelope(ciphertext, aad, "v2:")
}

func (s *GatewayStore) encryptSecretEnvelope(plaintext string, aad []byte, prefix string) (string, error) {
	gcm, err := s.secretGCM()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), aad)
	raw := append(nonce, sealed...)
	return prefix + base64.RawStdEncoding.EncodeToString(raw), nil
}

func (s *GatewayStore) decryptSecretEnvelope(ciphertext string, aad []byte, prefix string) (string, error) {
	ciphertext = strings.TrimSpace(ciphertext)
	if ciphertext == "" {
		return "", nil
	}
	if !strings.HasPrefix(ciphertext, prefix) {
		return "", fmt.Errorf("unsupported secret ciphertext")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(ciphertext, prefix))
	if err != nil {
		return "", err
	}
	gcm, err := s.secretGCM()
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("invalid secret ciphertext")
	}
	nonce := raw[:gcm.NonceSize()]
	sealed := raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *GatewayStore) secretGCM() (cipher.AEAD, error) {
	key, err := s.secretKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *GatewayStore) secretKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("DOOPS_GATEWAY_SECRET_KEY"))
	if raw == "" {
		return nil, ErrSecretKeyUnavailable
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}
