package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// IssueRequest 是平台无关的提 issue 请求。
type IssueRequest struct {
	Title  string
	Body   string
	Labels []string
}

// IssueResult 是提交成功后的最小返回。
type IssueResult struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// IssueClient 抽象不同 git 平台的提 issue 能力。
type IssueClient interface {
	CreateIssue(ctx context.Context, repoSlug string, req IssueRequest) (IssueResult, error)
}

var issueHTTPClient = &http.Client{Timeout: 30 * time.Second}

// NewIssueClient 按平台名构造对应的客户端。token 由调用方从环境变量解析后传入。
func NewIssueClient(platform, apiBase, token string) (IssueClient, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "", "cnb":
		base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
		if base == "" {
			base = "https://api.cnb.cool"
		}
		return &cnbIssueClient{apiBase: base, token: token}, nil
	case "github":
		base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
		if base == "" {
			base = "https://api.github.com"
		}
		return &githubIssueClient{apiBase: base, token: token}, nil
	default:
		return nil, fmt.Errorf("unsupported git platform: %s", platform)
	}
}

// --- CNB (cnb.cool) ---

type cnbIssueClient struct {
	apiBase string
	token   string
}

func (c *cnbIssueClient) CreateIssue(ctx context.Context, repoSlug string, req IssueRequest) (IssueResult, error) {
	repoSlug = strings.Trim(strings.TrimSpace(repoSlug), "/")
	if repoSlug == "" {
		return IssueResult{}, fmt.Errorf("cnb: repo slug is required (e.g. group/repo)")
	}
	payload := map[string]interface{}{"title": req.Title, "body": req.Body}
	if len(req.Labels) > 0 {
		payload["labels"] = req.Labels
	}
	body, _ := json.Marshal(payload)
	url := c.apiBase + "/" + repoSlug + "/-/issues"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return IssueResult{}, err
	}
	httpReq.Header.Set("Accept", "application/vnd.cnb.api+json")
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := issueHTTPClient.Do(httpReq)
	if err != nil {
		return IssueResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return IssueResult{}, fmt.Errorf("cnb create issue failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Number int `json:"number"`
		IID    int `json:"iid"`
	}
	_ = json.Unmarshal(raw, &parsed)
	number := parsed.Number
	if number == 0 {
		number = parsed.IID
	}
	// Issue 网页链接：<host>/<slug>/-/issues/<number>
	webHost := "https://cnb.cool"
	url = webHost + "/" + repoSlug + "/-/issues/"
	if number > 0 {
		url = fmt.Sprintf("%s%d", url, number)
	}
	return IssueResult{Number: number, URL: url}, nil
}

// --- GitHub ---

type githubIssueClient struct {
	apiBase string
	token   string
}

func (g *githubIssueClient) CreateIssue(ctx context.Context, repoSlug string, req IssueRequest) (IssueResult, error) {
	repoSlug = strings.Trim(strings.TrimSpace(repoSlug), "/")
	if !strings.Contains(repoSlug, "/") {
		return IssueResult{}, fmt.Errorf("github: repo slug must be owner/repo")
	}
	payload := map[string]interface{}{"title": req.Title, "body": req.Body}
	if len(req.Labels) > 0 {
		payload["labels"] = req.Labels
	}
	body, _ := json.Marshal(payload)
	url := g.apiBase + "/repos/" + repoSlug + "/issues"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return IssueResult{}, err
	}
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")
	if g.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := issueHTTPClient.Do(httpReq)
	if err != nil {
		return IssueResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return IssueResult{}, fmt.Errorf("github create issue failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return IssueResult{Number: parsed.Number, URL: parsed.HTMLURL}, nil
}
