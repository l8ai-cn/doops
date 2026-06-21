package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	IP          string   `json:"ip"`
	Port        string   `json:"port"`
	Use         string   `json:"use"`
	Token       string   `json:"token,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	Cluster     string   `json:"cluster,omitempty"`
	Instance    string   `json:"instance,omitempty"`
	SSHUser     string   `json:"ssh_user,omitempty"`
	SSHPort     string   `json:"ssh_port,omitempty"`
	SSHPassword string   `json:"ssh_password,omitempty"`
}

// GetSSHUser 返回 SSH 用户名，默认 root
func (s Server) GetSSHUser() string {
	if s.SSHUser != "" {
		return s.SSHUser
	}
	return "root"
}

// GetSSHPort 返回 SSH 端口，默认 22
func (s Server) GetSSHPort() string {
	if s.SSHPort != "" {
		return s.SSHPort
	}
	return "22"
}

type SessionInfo struct {
	ID        string  `json:"id"`
	Timestamp float64 `json:"timestamp"`
}

type SessionStore struct {
	Path     string
	Sessions map[string]SessionInfo
	mu       sync.Mutex
}

func NewSessionStore() *SessionStore {
	path := defaultSessionStorePath()

	ss := &SessionStore{
		Path:     path,
		Sessions: make(map[string]SessionInfo),
	}
	ss.load()
	return ss
}

func (ss *SessionStore) load() {
	data, err := os.ReadFile(ss.Path)
	if err != nil {
		return
	}

	var rawSessions map[string]SessionInfo
	if err := json.Unmarshal(data, &rawSessions); err != nil {
		return
	}

	now := float64(time.Now().Unix())
	for k, v := range rawSessions {
		if now-v.Timestamp < 3600 {
			ss.Sessions[k] = v
		}
	}
}

func (ss *SessionStore) Save() {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	data, _ := json.MarshalIndent(ss.Sessions, "", "    ")
	os.WriteFile(ss.Path, data, 0644)
}

func (ss *SessionStore) Get(target, sessionName string) string {
	key := target + ":" + sessionName
	info, ok := ss.Sessions[key]
	if !ok {
		return ""
	}
	return info.ID
}

func (ss *SessionStore) Set(target, sessionName, sessionID string) {
	key := target + ":" + sessionName
	ss.mu.Lock()
	ss.Sessions[key] = SessionInfo{
		ID:        sessionID,
		Timestamp: float64(time.Now().Unix()),
	}
	ss.mu.Unlock()
	ss.Save()
}

func (ss *SessionStore) GetOrCreate(target, sessionName string) string {
	sid := ss.Get(target, sessionName)
	if sid == "" {
		// The WebSocket gateway owns remote session mapping. Empty means the
		// next request should let the agent create or resume the remote session.
		return ""
	}
	return sid
}

type LLMConfig struct {
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	BaseURL string `json:"base_url"`
}

type RootConfig struct {
	Servers []Server  `json:"servers"`
	LLM     LLMConfig `json:"llm"`
}

// Global variable to hold loaded LLM config, to avoid changing all signatures at once if lazily needed
// But better to return it properly.

func doopsStateDir() string {
	return filepath.Dir(doopsConfigPath())
}

func ensureDoopsStateDir() string {
	dir := doopsStateDir()
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func defaultSessionStorePath() string {
	return filepath.Join(ensureDoopsStateDir(), "sessions.json")
}

func doopsConfigPath() string {
	if override := strings.TrimSpace(os.Getenv("DOOPS_CONFIG")); override != "" {
		return override
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agent", "skills", "doops", "config.json")
}

// LoadConfig returns Servers and LLMConfig
func LoadConfig() ([]Server, LLMConfig, error) {
	return loadPath(doopsConfigPath())
}

func loadPath(path string) ([]Server, LLMConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, LLMConfig{}, err
	}

	// Try New Format (Object)
	var root RootConfig
	if err := json.Unmarshal(data, &root); err == nil && len(root.Servers) > 0 {
		return root.Servers, root.LLM, nil
	}

	// Try Old Format (Array)
	var servers []Server
	if err := json.Unmarshal(data, &servers); err == nil {
		return servers, LLMConfig{}, nil
	}

	// Try New Format again (maybe servers empty but LLM exists?)
	if err := json.Unmarshal(data, &root); err == nil {
		return root.Servers, root.LLM, nil
	}

	return nil, LLMConfig{}, fmt.Errorf("failed to parse config at %s", path)
}

func LoadServers() ([]Server, error) {
	servers, _, err := LoadConfig()
	return servers, err
}

func GetLLMConfig() LLMConfig {
	_, llm, _ := LoadConfig()
	return llm
}

func saveServers(servers []Server) error {
	// We need to preserve LLM config if getting passed only servers
	_, currentLLM, _ := LoadConfig()

	configPath := doopsConfigPath()

	root := RootConfig{
		Servers: normalizeServersForSave(servers),
		LLM:     currentLLM,
	}

	data, err := json.MarshalIndent(root, "", "    ")
	if err != nil {
		return err
	}

	os.MkdirAll(filepath.Dir(configPath), 0755)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return err
	}
	return os.Chmod(configPath, 0600)
}

func normalizeServersForSave(servers []Server) []Server {
	normalized := make([]Server, 0, len(servers))
	for _, server := range servers {
		server.Aliases = normalizeAliases(server.Aliases)
		normalized = append(normalized, server)
	}
	return normalized
}

func normalizeAliases(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			alias := strings.TrimSpace(part)
			if alias == "" || seen[alias] {
				continue
			}
			seen[alias] = true
			out = append(out, alias)
		}
	}
	return out
}
