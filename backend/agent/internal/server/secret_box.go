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
	"path/filepath"
	"strings"
)

func (s *GatewayStore) encryptSecret(plaintext string) (string, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return "", nil
	}
	gcm, err := s.secretGCM()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	raw := append(nonce, sealed...)
	return "v1:" + base64.RawStdEncoding.EncodeToString(raw), nil
}

func (s *GatewayStore) decryptSecret(ciphertext string) (string, error) {
	ciphertext = strings.TrimSpace(ciphertext)
	if ciphertext == "" {
		return "", nil
	}
	if !strings.HasPrefix(ciphertext, "v1:") {
		return "", fmt.Errorf("unsupported secret ciphertext")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(ciphertext, "v1:"))
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
	plain, err := gcm.Open(nil, nonce, sealed, nil)
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
	if raw := strings.TrimSpace(os.Getenv("DOOPS_GATEWAY_SECRET_KEY")); raw != "" {
		sum := sha256.Sum256([]byte(raw))
		return sum[:], nil
	}
	path := s.secretKeyPath()
	if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) != "" {
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(raw))))
		return sum[:], nil
	}
	secret := randomHex(32)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

func (s *GatewayStore) secretKeyPath() string {
	var dbPath string
	if s != nil && s.db != nil {
		_ = s.db.QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &dbPath)
	}
	if strings.TrimSpace(dbPath) == "" {
		dbPath = "/var/lib/doops-gateway/gateway.db"
	}
	return filepath.Join(filepath.Dir(dbPath), "gateway.secret")
}
