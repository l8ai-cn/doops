package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AuthStore struct {
	Tokens map[string]string `json:"tokens"`
}

func GetAuthPath() string {
	return filepath.Join(ensureDoopsStateDir(), "auth.json")
}

func LoadAuth() (*AuthStore, error) {
	path := GetAuthPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return &AuthStore{Tokens: make(map[string]string)}, nil
	}

	var store AuthStore
	if err := json.Unmarshal(data, &store); err != nil {
		return &AuthStore{Tokens: make(map[string]string)}, nil
	}

	if store.Tokens == nil {
		store.Tokens = make(map[string]string)
	}
	return &store, nil
}

func (s *AuthStore) Save() error {
	path := GetAuthPath()
	data, err := json.MarshalIndent(s, "", "    ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func (s *AuthStore) Get(target string) string {
	return s.Tokens[target]
}

func (s *AuthStore) Set(target, token string) {
	if s.Tokens == nil {
		s.Tokens = make(map[string]string)
	}
	s.Tokens[target] = token
}

func (s *AuthStore) Remove(target string) {
	delete(s.Tokens, target)
}

type gatewayLoginResponse struct {
	Token    string `json:"token"`
	TokenID  string `json:"token_id"`
	Username string `json:"username"`
}

type gatewayAdminTokenCreateRequest struct {
	User    string `json:"user"`
	Name    string `json:"name,omitempty"`
	Expires string `json:"expires,omitempty"`
}

type gatewayAdminTokenCreateResponse struct {
	Token     string `json:"token"`
	TokenID   string `json:"token_id"`
	TokenType string `json:"token_type"`
	Username  string `json:"username"`
}

type gatewayActiveOperation struct {
	ID             string `json:"id"`
	UserID         string `json:"user_id"`
	TokenID        string `json:"token_id,omitempty"`
	Cluster        string `json:"cluster"`
	Instance       string `json:"instance"`
	Action         string `json:"action"`
	Session        string `json:"session,omitempty"`
	CommandSummary string `json:"command_summary,omitempty"`
	Kind           string `json:"kind"`
	StartedAt      string `json:"started_at"`
	AgeSeconds     int64  `json:"age_seconds"`
}

type gatewayAdminOperationsResponse struct {
	Operations []gatewayActiveOperation `json:"operations"`
}

func GatewayLogin(gateway, username, password, label string) (string, error) {
	loginURL, err := gatewayAuthLoginURL(gateway)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
		"name":     label,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, loginURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway login failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var parsed gatewayLoginResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.Token) == "" {
		return "", fmt.Errorf("gateway login returned empty token")
	}
	return parsed.Token, nil
}

func GatewayAdminTokenCreate(gateway, adminToken string, create gatewayAdminTokenCreateRequest) (gatewayAdminTokenCreateResponse, error) {
	create.User = strings.TrimSpace(create.User)
	create.Name = strings.TrimSpace(create.Name)
	create.Expires = strings.TrimSpace(create.Expires)
	if create.User == "" {
		return gatewayAdminTokenCreateResponse{}, fmt.Errorf("user is required")
	}
	createURL, err := gatewayAdminTokenCreateURL(gateway)
	if err != nil {
		return gatewayAdminTokenCreateResponse{}, err
	}
	body, err := json.Marshal(create)
	if err != nil {
		return gatewayAdminTokenCreateResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, createURL, bytes.NewReader(body))
	if err != nil {
		return gatewayAdminTokenCreateResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(adminToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(adminToken))
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return gatewayAdminTokenCreateResponse{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return gatewayAdminTokenCreateResponse{}, fmt.Errorf("gateway admin token create failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var parsed gatewayAdminTokenCreateResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return gatewayAdminTokenCreateResponse{}, err
	}
	if strings.TrimSpace(parsed.Token) == "" {
		return gatewayAdminTokenCreateResponse{}, fmt.Errorf("gateway admin token create returned empty token")
	}
	return parsed, nil
}

func GatewayAdminOperationsList(gateway, adminToken string) ([]gatewayActiveOperation, error) {
	opsURL, err := gatewayAdminOperationsURL(gateway, "")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, opsURL, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(adminToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(adminToken))
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway admin operations list failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var parsed gatewayAdminOperationsResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	return parsed.Operations, nil
}

func GatewayAdminOperationCancel(gateway, adminToken, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("operation id is required")
	}
	cancelURL, err := gatewayAdminOperationsURL(gateway, id)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, cancelURL, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(adminToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(adminToken))
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway admin operation cancel failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func gatewayAuthLoginURL(rawGateway string) (string, error) {
	rawGateway = strings.TrimSpace(rawGateway)
	if rawGateway == "" {
		return "", fmt.Errorf("gateway URL is required")
	}
	if !strings.Contains(rawGateway, "://") {
		rawGateway = "https://" + rawGateway
	}
	u, err := url.Parse(rawGateway)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported gateway scheme %q", u.Scheme)
	}
	if err := enforceSecureGatewayURL(rawGateway, u); err != nil {
		return "", err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/auth/login"
	}
	u.RawQuery = ""
	return u.String(), nil
}

func gatewayAdminTokenCreateURL(rawGateway string) (string, error) {
	return gatewayURLWithPath(rawGateway, "/v1/admin/tokens", nil)
}

func gatewayAdminOperationsURL(rawGateway, id string) (string, error) {
	values := url.Values{}
	if strings.TrimSpace(id) != "" {
		values.Set("id", strings.TrimSpace(id))
	}
	return gatewayURLWithPath(rawGateway, "/v1/admin/operations", values)
}
