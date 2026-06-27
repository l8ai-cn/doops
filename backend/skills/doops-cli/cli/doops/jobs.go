package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type schedulerJob struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ClusterGlob    string `json:"cluster_glob"`
	InstanceGlob   string `json:"instance_glob"`
	IntervalSec    int64  `json:"interval_sec"`
	ScanMode       string `json:"scan_mode"`
	ScanConfig     string `json:"scan_config"`
	Platform       string `json:"platform"`
	RepoSlug       string `json:"repo_slug"`
	Labels         string `json:"labels"`
	TokenEnv       string `json:"token_env"`
	APIBase        string `json:"api_base"`
	DedupWindowSec int64  `json:"dedup_window_sec"`
	Enabled        bool   `json:"enabled"`
	LastRunAt      string `json:"last_run_at"`
	CreatedAt      string `json:"created_at"`
}

type schedulerIssue struct {
	JobID     string `json:"job_id"`
	RepoSlug  string `json:"repo_slug"`
	Cluster   string `json:"cluster"`
	Instance  string `json:"instance"`
	IssueURL  string `json:"issue_url"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// runAdminJobs 派发 `doops admin jobs <sub>` 子命令。
func runAdminJobs(args []string, servers []Server, configErr error) {
	if len(args) == 0 {
		printJobsUsage()
		os.Exit(1)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		gateway, token := resolveAdminGatewayFlags("admin jobs list", rest, servers, configErr, nil)
		jobs, err := gatewayJobsList(gateway, token)
		exitOnErr(err)
		printJobs(jobs)
	case "add":
		runAdminJobAdd(rest, servers, configErr)
	case "rm", "delete":
		var id string
		gateway, token := resolveAdminGatewayFlags("admin jobs rm", rest, servers, configErr, &id)
		exitOnErr(gatewayJobDelete(gateway, token, id))
		fmt.Printf("Deleted job %s\n", id)
	case "run":
		var id string
		gateway, token := resolveAdminGatewayFlags("admin jobs run", rest, servers, configErr, &id)
		summary, err := gatewayJobRun(gateway, token, id, "")
		exitOnErr(err)
		fmt.Println(summary)
	case "enable", "disable":
		var id string
		gateway, token := resolveAdminGatewayFlags("admin jobs "+sub, rest, servers, configErr, &id)
		exitOnErr(gatewayJobSetEnabled(gateway, token, id, sub == "enable"))
		fmt.Printf("Job %s %sd\n", id, sub)
	case "issues":
		runAdminJobIssues(rest, servers, configErr)
	default:
		printJobsUsage()
		os.Exit(1)
	}
}

func runAdminJobAdd(args []string, servers []Server, configErr error) {
	var target, gateway, token string
	var name, repo, platform, clusterGlob, instanceGlob, scanMode string
	var instruction, command, labels, tokenEnv, apiBase string
	var interval, dedup string
	var disabled bool
	fs := flag.NewFlagSet("admin jobs add", flag.ExitOnError)
	fs.StringVar(&target, "target", "", "Configured gateway target")
	fs.StringVar(&gateway, "gateway", "", "Gateway URL")
	fs.StringVar(&token, "token", "", "Gateway admin user token")
	fs.StringVar(&name, "name", "", "Job name (required)")
	fs.StringVar(&repo, "repo", "", "Issue repo slug, e.g. group/repo (required)")
	fs.StringVar(&platform, "platform", "cnb", "Git platform: cnb | github")
	fs.StringVar(&clusterGlob, "cluster-glob", "*", "Cluster glob to match")
	fs.StringVar(&instanceGlob, "instance-glob", "*", "Instance glob to match")
	fs.StringVar(&scanMode, "scan-mode", "ask", "Scan mode: ask | exec | audit")
	fs.StringVar(&instruction, "instruction", "", "ask-mode scan instruction override")
	fs.StringVar(&command, "command", "", "exec-mode scan shell command override")
	fs.StringVar(&labels, "labels", "", "Comma separated issue labels")
	fs.StringVar(&tokenEnv, "token-env", "", "Env var on gateway host holding the git token, e.g. CNB_TOKEN")
	fs.StringVar(&apiBase, "api-base", "", "Override platform API base URL")
	fs.StringVar(&interval, "interval", "1h", "Scan interval, e.g. 30m, 1h")
	fs.StringVar(&dedup, "dedup", "24h", "Dedup window for the same fingerprint, e.g. 24h")
	fs.BoolVar(&disabled, "disabled", false, "Create the job in disabled state")
	fs.Parse(args)

	gateway, token = applyTargetDefaults(target, gateway, token, servers, configErr)
	if gateway == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(repo) == "" {
		fmt.Println("Error: --gateway/--target, --name and --repo are required")
		fs.Usage()
		os.Exit(1)
	}

	intervalSec := mustDurationSeconds("interval", interval)
	dedupSec := mustDurationSeconds("dedup", dedup)

	scanConfig := map[string]string{}
	if strings.TrimSpace(instruction) != "" {
		scanConfig["instruction"] = instruction
	}
	if strings.TrimSpace(command) != "" {
		scanConfig["command"] = command
	}
	scanConfigRaw, _ := json.Marshal(scanConfig)

	enabled := !disabled
	body := map[string]interface{}{
		"name":             name,
		"cluster_glob":     clusterGlob,
		"instance_glob":    instanceGlob,
		"interval_sec":     intervalSec,
		"scan_mode":        scanMode,
		"scan_config":      json.RawMessage(scanConfigRaw),
		"platform":         platform,
		"repo_slug":        repo,
		"labels":           labels,
		"token_env":        tokenEnv,
		"api_base":         apiBase,
		"dedup_window_sec": dedupSec,
		"enabled":          enabled,
	}
	job, err := gatewayJobCreate(gateway, token, body)
	exitOnErr(err)
	fmt.Printf("Created job %s (%s)\n", job.ID, job.Name)
	if strings.TrimSpace(tokenEnv) == "" && scanMode != "audit" {
		fmt.Println("note: --token-env 未设置，提 issue 时 gateway 主机需要有对应的 git token 环境变量")
	}
}

func runAdminJobIssues(args []string, servers []Server, configErr error) {
	var target, gateway, token, id string
	var limit int
	fs := flag.NewFlagSet("admin jobs issues", flag.ExitOnError)
	fs.StringVar(&target, "target", "", "Configured gateway target")
	fs.StringVar(&gateway, "gateway", "", "Gateway URL")
	fs.StringVar(&token, "token", "", "Gateway admin user token")
	fs.StringVar(&id, "id", "", "Optional job id filter")
	fs.IntVar(&limit, "limit", 50, "Max rows")
	fs.Parse(args)
	gateway, token = applyTargetDefaults(target, gateway, token, servers, configErr)
	if gateway == "" {
		fmt.Println("Error: --gateway or --target is required")
		os.Exit(1)
	}
	issues, err := gatewayJobIssues(gateway, token, id, limit)
	exitOnErr(err)
	fmt.Printf("%-22s %-18s %-10s %-8s %s\n", "CREATED", "CLUSTER/INSTANCE", "STATUS", "JOB", "ISSUE")
	fmt.Println(strings.Repeat("-", 110))
	for _, is := range issues {
		loc := is.Cluster + "/" + is.Instance
		link := is.IssueURL
		if link == "" {
			link = "(" + is.Title + ")"
		}
		fmt.Printf("%-22s %-18s %-10s %-8s %s\n", is.CreatedAt, loc, is.Status, is.JobID, link)
	}
}

func printJobs(jobs []schedulerJob) {
	fmt.Printf("%-16s %-18s %-9s %-7s %-22s %-9s %s\n", "ID", "NAME", "MODE", "PLAT", "REPO", "ENABLED", "SCOPE")
	fmt.Println(strings.Repeat("-", 120))
	for _, j := range jobs {
		scope := j.ClusterGlob + "/" + j.InstanceGlob + " every " + (time.Duration(j.IntervalSec) * time.Second).String()
		fmt.Printf("%-16s %-18s %-9s %-7s %-22s %-9v %s\n", j.ID, truncate(j.Name, 18), j.ScanMode, j.Platform, truncate(j.RepoSlug, 22), j.Enabled, scope)
	}
}

func printJobsUsage() {
	fmt.Println("Usage:")
	fmt.Println("  doops admin jobs list   --target <gw>")
	fmt.Println("  doops admin jobs add    --target <gw> --name <n> --repo group/repo [--platform cnb|github]")
	fmt.Println("                          [--scan-mode ask|exec|audit] [--cluster-glob *] [--instance-glob *]")
	fmt.Println("                          [--interval 1h] [--dedup 24h] [--labels a,b] [--token-env CNB_TOKEN]")
	fmt.Println("                          [--instruction '...'] [--command '...'] [--api-base url] [--disabled]")
	fmt.Println("  doops admin jobs run    --target <gw> --id <id>")
	fmt.Println("  doops admin jobs enable|disable --target <gw> --id <id>")
	fmt.Println("  doops admin jobs rm     --target <gw> --id <id>")
	fmt.Println("  doops admin jobs issues --target <gw> [--id <id>] [--limit N]")
}

/* ---- HTTP client ---- */

func gatewayJobsList(gateway, token string) ([]schedulerJob, error) {
	raw, err := gatewayAdminRequest(http.MethodGet, gateway, token, "/v1/admin/jobs", nil, nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Jobs []schedulerJob `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Jobs, nil
}

func gatewayJobCreate(gateway, token string, body map[string]interface{}) (schedulerJob, error) {
	payload, _ := json.Marshal(body)
	raw, err := gatewayAdminRequest(http.MethodPost, gateway, token, "/v1/admin/jobs", nil, payload)
	if err != nil {
		return schedulerJob{}, err
	}
	var job schedulerJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return schedulerJob{}, err
	}
	return job, nil
}

func gatewayJobDelete(gateway, token, id string) error {
	_, err := gatewayAdminRequest(http.MethodDelete, gateway, token, "/v1/admin/jobs", url.Values{"id": []string{id}}, nil)
	return err
}

func gatewayJobRun(gateway, token, id, _ string) (string, error) {
	raw, err := gatewayAdminRequest(http.MethodPost, gateway, token, "/v1/admin/jobs/run", url.Values{"id": []string{id}}, nil)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if parsed.Summary == "" {
		return "job triggered", nil
	}
	return parsed.Summary, nil
}

func gatewayJobSetEnabled(gateway, token, id string, enabled bool) error {
	q := url.Values{"id": []string{id}, "enabled": []string{fmt.Sprintf("%v", enabled)}}
	_, err := gatewayAdminRequest(http.MethodPost, gateway, token, "/v1/admin/jobs/run", q, nil)
	return err
}

func gatewayJobIssues(gateway, token, id string, limit int) ([]schedulerIssue, error) {
	q := url.Values{}
	if strings.TrimSpace(id) != "" {
		q.Set("id", id)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	raw, err := gatewayAdminRequest(http.MethodGet, gateway, token, "/v1/admin/jobs/issues", q, nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Issues []schedulerIssue `json:"issues"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Issues, nil
}

func gatewayAdminRequest(method, gateway, token, endpoint string, query url.Values, body []byte) ([]byte, error) {
	reqURL, err := gatewayURLWithPath(gateway, endpoint, query)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, reqURL, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := (&http.Client{Timeout: 11 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gateway request failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

/* ---- shared helpers ---- */

func applyTargetDefaults(target, gateway, token string, servers []Server, configErr error) (string, string) {
	if target != "" {
		requireConfig(configErr)
		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}
		if gateway == "" {
			gateway = server.Gateway
		}
		if token == "" {
			token = ResolveToken(server.Name, server.Token)
		}
	}
	return gateway, token
}

func mustDurationSeconds(name, raw string) int64 {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		fmt.Printf("Error: invalid --%s duration %q\n", name, raw)
		os.Exit(1)
	}
	return int64(d.Seconds())
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
