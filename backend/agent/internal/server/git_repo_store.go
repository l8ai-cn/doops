package server

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// GitRepo is a stored deployable source repository visible in the admin UI.
// Password material is never returned to clients.
type GitRepo struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	URL                string     `json:"url"`
	Branch             string     `json:"branch"`
	Username           string     `json:"username"`
	PasswordHash       string     `json:"-"`
	PasswordCiphertext string     `json:"-"`
	HasPassword        bool       `json:"has_password"`
	Description        string     `json:"description"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

type GitRepoInput struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Branch      string `json:"branch"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Description string `json:"description"`
}

func (s *GatewayStore) ListGitRepos() ([]GitRepo, error) {
	rows, err := s.db.Query(`SELECT id, name, url, branch, username, password_hash, password_ciphertext, description, last_used_at, created_at
		FROM git_repos ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []GitRepo
	for rows.Next() {
		repo, err := scanGitRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

func (s *GatewayStore) CreateGitRepo(input GitRepoInput) (GitRepo, error) {
	repo, password, err := normalizeGitRepoInput(input, true)
	if err != nil {
		return GitRepo{}, err
	}
	passwordCiphertext, err := s.encryptSecret(password)
	if err != nil {
		return GitRepo{}, err
	}
	now := time.Now().UTC()
	repo.ID = "repo_" + randomHex(12)
	repo.CreatedAt = now
	repo.PasswordCiphertext = passwordCiphertext
	_, err = s.db.Exec(`INSERT INTO git_repos
		(id, name, url, branch, username, password_hash, password_ciphertext, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repo.ID, repo.Name, repo.URL, repo.Branch, repo.Username, "", repo.PasswordCiphertext,
		repo.Description, formatTime(now), formatTime(now))
	if err != nil {
		return GitRepo{}, err
	}
	repo.HasPassword = repo.PasswordCiphertext != ""
	return repo, nil
}

func (s *GatewayStore) UpdateGitRepo(id string, input GitRepoInput) (GitRepo, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return GitRepo{}, fmt.Errorf("repo id is required")
	}
	current, err := s.GetGitRepo(id)
	if err != nil {
		return GitRepo{}, err
	}
	if v := strings.TrimSpace(input.Name); v != "" {
		current.Name = v
	}
	if v := strings.TrimSpace(input.URL); v != "" {
		current.URL = v
	}
	if v := strings.TrimSpace(input.Branch); v != "" {
		current.Branch = v
	}
	if input.Username != "" {
		current.Username = strings.TrimSpace(input.Username)
	}
	if input.Description != "" {
		current.Description = strings.TrimSpace(input.Description)
	}
	if strings.TrimSpace(input.Password) != "" {
		ciphertext, err := s.encryptSecret(input.Password)
		if err != nil {
			return GitRepo{}, err
		}
		current.PasswordHash = ""
		current.PasswordCiphertext = ciphertext
	}
	if err := validateGitRepo(current.Name, current.URL, current.Branch); err != nil {
		return GitRepo{}, err
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE git_repos SET
		name = ?, url = ?, branch = ?, username = ?, password_hash = ?, password_ciphertext = ?, description = ?, updated_at = ?
		WHERE id = ?`,
		current.Name, current.URL, current.Branch, current.Username, current.PasswordHash, current.PasswordCiphertext,
		current.Description, formatTime(now), id)
	if err != nil {
		return GitRepo{}, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return GitRepo{}, sql.ErrNoRows
	}
	current.HasPassword = current.PasswordCiphertext != "" || current.PasswordHash != ""
	return current, nil
}

func (s *GatewayStore) DeleteGitRepo(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("repo id is required")
	}
	res, err := s.db.Exec(`DELETE FROM git_repos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *GatewayStore) GetGitRepo(id string) (GitRepo, error) {
	row := s.db.QueryRow(`SELECT id, name, url, branch, username, password_hash, password_ciphertext, description, last_used_at, created_at
		FROM git_repos WHERE id = ?`, strings.TrimSpace(id))
	return scanGitRepo(row)
}

func (s *GatewayStore) GitRepoPassword(id string) (string, error) {
	var ciphertext string
	err := s.db.QueryRow(`SELECT password_ciphertext FROM git_repos WHERE id = ?`, strings.TrimSpace(id)).Scan(&ciphertext)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ciphertext) == "" {
		return "", nil
	}
	return s.decryptSecret(ciphertext)
}

func (s *GatewayStore) MarkGitRepoUsed(id string, at time.Time) (GitRepo, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.Exec(`UPDATE git_repos SET last_used_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(at.UTC()), formatTime(at.UTC()), strings.TrimSpace(id))
	if err != nil {
		return GitRepo{}, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return GitRepo{}, sql.ErrNoRows
	}
	return s.GetGitRepo(id)
}

type gitRepoScanner interface {
	Scan(dest ...any) error
}

func scanGitRepo(scanner gitRepoScanner) (GitRepo, error) {
	var repo GitRepo
	var passwordHash, passwordCiphertext, lastUsed, created string
	if err := scanner.Scan(&repo.ID, &repo.Name, &repo.URL, &repo.Branch, &repo.Username,
		&passwordHash, &passwordCiphertext, &repo.Description, &lastUsed, &created); err != nil {
		return GitRepo{}, err
	}
	repo.PasswordHash = passwordHash
	repo.PasswordCiphertext = passwordCiphertext
	repo.HasPassword = strings.TrimSpace(passwordCiphertext) != "" || strings.TrimSpace(passwordHash) != ""
	if lastUsed != "" {
		if parsed, err := parseTime(lastUsed); err == nil {
			repo.LastUsedAt = &parsed
		}
	}
	repo.CreatedAt, _ = parseTime(created)
	return repo, nil
}

func normalizeGitRepoInput(input GitRepoInput, requireURL bool) (GitRepo, string, error) {
	name := strings.TrimSpace(input.Name)
	repoURL := strings.TrimSpace(input.URL)
	branch := strings.TrimSpace(input.Branch)
	if branch == "" {
		branch = "main"
	}
	if err := validateGitRepo(name, repoURL, branch); err != nil {
		if requireURL || repoURL != "" || name != "" {
			return GitRepo{}, "", err
		}
	}
	password := ""
	if strings.TrimSpace(input.Password) != "" {
		password = strings.TrimSpace(input.Password)
	}
	return GitRepo{
		Name:        name,
		URL:         repoURL,
		Branch:      branch,
		Username:    strings.TrimSpace(input.Username),
		Description: strings.TrimSpace(input.Description),
	}, password, nil
}

func validateGitRepo(name, repoURL, branch string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("repo name is required")
	}
	if strings.TrimSpace(repoURL) == "" {
		return fmt.Errorf("repo url is required")
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("repo branch is required")
	}
	return nil
}
