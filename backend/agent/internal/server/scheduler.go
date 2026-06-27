package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"time"
)

// Scheduler 周期性执行 scheduler_jobs：扫描匹配实例的运行时异常，归一化后到
// 对应 git 平台提 issue，并按 fingerprint 去重。它运行在 gateway 进程内，复用
// hub 的在线连接与目标串行语义。
type Scheduler struct {
	hub      *GatewayHub
	store    *GatewayStore
	interval time.Duration
	stop     chan struct{}
}

// scanException 是单条归一化后的异常。
type scanException struct {
	Title       string `json:"title"`
	Fingerprint string `json:"fingerprint"`
	Severity    string `json:"severity"`
	Detail      string `json:"detail"`
}

const defaultAskScanInstruction = "你是只读运维巡检助手，只允许读取信息，禁止任何修改类操作。" +
	"请检查本节点最近的运行时异常（容器/Pod 崩溃重启、CrashLoopBackOff、OOMKilled、panic、" +
	"Error/Exception 堆栈、服务不可用等）。把发现的异常归纳为一个 JSON 数组输出，" +
	"每个元素包含字段：title(简短标题)、fingerprint(同类异常稳定指纹，用于去重，相同问题必须给相同值)、" +
	"severity(low|medium|high)、detail(关键证据或日志摘要)。" +
	"严格只输出 JSON 数组本身，不要输出额外解释或 markdown 代码块。若没有异常，输出 []。"

const defaultExecScanCommand = "sh -c '" +
	"kubectl get pods -A 2>/dev/null | grep -Ei \"crashloop|oomkill|error|err\\b\" | head -50; " +
	"(docker ps -a 2>/dev/null || nerdctl ps -a 2>/dev/null) | grep -Ei \"exited \\([1-9]|restarting\" | head -50" +
	"'"

func NewScheduler(hub *GatewayHub, store *GatewayStore, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Scheduler{hub: hub, store: store, interval: interval, stop: make(chan struct{})}
}

func (s *Scheduler) Start() {
	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		log.Printf("[scheduler] ✅ started (tick=%s)", s.interval)
		for {
			select {
			case <-ticker.C:
				s.tick()
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	close(s.stop)
}

func (s *Scheduler) tick() {
	jobs, err := s.store.DueSchedulerJobs(time.Now().UTC())
	if err != nil {
		log.Printf("[scheduler] list due jobs failed: %v", err)
		return
	}
	for _, job := range jobs {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		created, err := s.runJob(ctx, job)
		cancel()
		_ = s.store.TouchSchedulerJobRun(job.ID, time.Now().UTC())
		if err != nil {
			log.Printf("[scheduler] job %s (%s) error: %v", job.ID, job.Name, err)
			continue
		}
		log.Printf("[scheduler] job %s (%s) done, %d new issue(s)", job.ID, job.Name, created)
	}
}

// RunJobNow 立即执行一个任务（供 run-now 接口使用），并返回人类可读摘要。
func (s *Scheduler) RunJobNow(ctx context.Context, jobID string) (string, error) {
	job, err := s.store.GetSchedulerJob(jobID)
	if err != nil {
		return "", err
	}
	created, err := s.runJob(ctx, job)
	_ = s.store.TouchSchedulerJobRun(job.ID, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("job %s 执行完成，新建 %d 个 issue", job.Name, created), nil
}

func (s *Scheduler) runJob(ctx context.Context, job SchedulerJob) (int, error) {
	var exceptions []scopedException
	var scanErr error
	switch strings.ToLower(job.ScanMode) {
	case "audit":
		exceptions, scanErr = s.scanAudit(job)
	case "exec":
		exceptions, scanErr = s.scanAgents(ctx, job, false)
	case "ask", "":
		exceptions, scanErr = s.scanAgents(ctx, job, true)
	default:
		return 0, fmt.Errorf("unknown scan_mode: %s", job.ScanMode)
	}
	if scanErr != nil {
		return 0, scanErr
	}
	if len(exceptions) == 0 {
		return 0, nil
	}

	token := ""
	if env := strings.TrimSpace(job.TokenEnv); env != "" {
		token = strings.TrimSpace(os.Getenv(env))
	}
	client, err := NewIssueClient(job.Platform, job.APIBase, token)
	if err != nil {
		return 0, err
	}

	labels := splitLabels(job.Labels)
	dedupSince := time.Now().UTC().Add(-time.Duration(job.DedupWindowSec) * time.Second)
	created := 0
	for _, exc := range exceptions {
		fp := compositeFingerprint(job.ID, exc.Cluster, exc.Instance, exc.Fingerprint, exc.Title)
		if _, found, err := s.store.FindRecentIssue(job.RepoSlug, fp, dedupSince); err != nil {
			log.Printf("[scheduler] dedup check failed: %v", err)
			continue
		} else if found {
			continue
		}
		title, body := renderIssue(job, exc)
		res, err := client.CreateIssue(ctx, job.RepoSlug, IssueRequest{Title: title, Body: body, Labels: labels})
		if err != nil {
			log.Printf("[scheduler] create issue failed (job=%s): %v", job.ID, err)
			// 记录为 failed，避免去重窗口内反复重试同一异常打爆平台。
			_, _ = s.store.RecordIssue(SchedulerIssue{
				JobID: job.ID, Fingerprint: fp, RepoSlug: job.RepoSlug,
				Cluster: exc.Cluster, Instance: exc.Instance, Title: title, Status: "failed",
			})
			continue
		}
		_, _ = s.store.RecordIssue(SchedulerIssue{
			JobID: job.ID, Fingerprint: fp, RepoSlug: job.RepoSlug,
			Cluster: exc.Cluster, Instance: exc.Instance, IssueURL: res.URL, Title: title, Status: "created",
		})
		created++
	}
	return created, nil
}

type scopedException struct {
	scanException
	Cluster  string
	Instance string
}

// scanAgents 对每个匹配的在线 agent 发起一次只读扫描（ask 或 exec）。
func (s *Scheduler) scanAgents(ctx context.Context, job SchedulerJob, useAsk bool) ([]scopedException, error) {
	targets := s.matchTargets(job)
	if len(targets) == 0 {
		return nil, nil
	}
	session := "sched-" + job.ID
	var out []scopedException
	for _, t := range targets {
		var text string
		var err error
		if useAsk {
			instruction := scanConfigString(job.ScanConfig, "instruction", defaultAskScanInstruction)
			text, err = s.hub.RunInternalToolCall(ctx, t.Cluster, t.Instance, "doops_agent_prompt", map[string]interface{}{
				"instruction": instruction,
				"session_id":  session,
			})
		} else {
			command := scanConfigString(job.ScanConfig, "command", defaultExecScanCommand)
			text, err = s.hub.RunInternalToolCall(ctx, t.Cluster, t.Instance, "doops_shell", map[string]interface{}{
				"command":    command,
				"session_id": session,
			})
		}
		if err != nil {
			log.Printf("[scheduler] scan %s/%s failed: %v", t.Cluster, t.Instance, err)
			continue
		}
		var excs []scanException
		if useAsk {
			excs = parseExceptionJSON(text)
		} else {
			excs = exceptionsFromLines(text)
		}
		for _, e := range excs {
			out = append(out, scopedException{scanException: e, Cluster: t.Cluster, Instance: t.Instance})
		}
	}
	return out, nil
}

// scanAudit 从 gateway 审计库里抽取窗口内 status=error 的操作作为异常。
func (s *Scheduler) scanAudit(job SchedulerJob) ([]scopedException, error) {
	events, err := s.store.ListAuditFiltered(AuditFilter{Status: "error", Limit: 500})
	if err != nil {
		return nil, err
	}
	since := time.Now().UTC().Add(-time.Duration(job.DedupWindowSec) * time.Second)
	var out []scopedException
	for _, ev := range events {
		if ev.StartedAt.Before(since) {
			continue
		}
		if !globMatch(job.ClusterGlob, ev.Cluster) || !globMatch(job.InstanceGlob, ev.Instance) {
			continue
		}
		title := fmt.Sprintf("[%s/%s] %s 操作失败", ev.Cluster, ev.Instance, ev.Action)
		detail := fmt.Sprintf("action=%s\nsession=%s\ncommand=%s\nerror=%s\ntail=%s",
			ev.Action, ev.Session, ev.CommandSummary, ev.Error, ev.Tail)
		out = append(out, scopedException{
			scanException: scanException{
				Title:       title,
				Fingerprint: string(ev.Action) + "|" + ev.CommandSummary,
				Severity:    "medium",
				Detail:      detail,
			},
			Cluster:  ev.Cluster,
			Instance: ev.Instance,
		})
	}
	return out, nil
}

func (s *Scheduler) matchTargets(job SchedulerJob) []GatewayTarget {
	all := s.hub.ListTargets()
	var matched []GatewayTarget
	for _, t := range all {
		if globMatch(job.ClusterGlob, t.Cluster) && globMatch(job.InstanceGlob, t.Instance) {
			matched = append(matched, t)
		}
	}
	return matched
}

/* ---- helpers ---- */

func renderIssue(job SchedulerJob, exc scopedException) (string, string) {
	title := exc.Title
	if title == "" {
		title = "运行时异常"
	}
	sev := exc.Severity
	if sev == "" {
		sev = "medium"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "> 由 doops 定时巡检任务 `%s` 自动创建\n\n", job.Name)
	fmt.Fprintf(&b, "- 集群/实例: `%s/%s`\n", exc.Cluster, exc.Instance)
	fmt.Fprintf(&b, "- 严重级别: `%s`\n", sev)
	fmt.Fprintf(&b, "- 发现时间: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- 扫描模式: `%s`\n\n", job.ScanMode)
	b.WriteString("## 详情\n\n")
	detail := strings.TrimSpace(exc.Detail)
	if detail == "" {
		detail = "(无更多详情)"
	}
	b.WriteString("```\n")
	b.WriteString(detail)
	b.WriteString("\n```\n")
	return title, b.String()
}

func splitLabels(raw string) []string {
	out := []string{"doops-auto"}
	for _, l := range strings.Split(raw, ",") {
		l = strings.TrimSpace(l)
		if l != "" && l != "doops-auto" {
			out = append(out, l)
		}
	}
	return out
}

func compositeFingerprint(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(strings.ToLower(strings.TrimSpace(p))))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func globMatch(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, strings.TrimSpace(name))
	if err != nil {
		return pattern == name
	}
	return ok
}

func scanConfigString(raw, key, fallback string) string {
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) == nil {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return fallback
}

// parseExceptionJSON 从 ask 返回的文本中提取 JSON 数组并解析为异常列表。
// doagent 输出可能包含前后缀或 markdown 代码块，因此先定位最外层 [ ... ]。
func parseExceptionJSON(text string) []scanException {
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end <= start {
		return nil
	}
	var excs []scanException
	if err := json.Unmarshal([]byte(text[start:end+1]), &excs); err != nil {
		return nil
	}
	var out []scanException
	for _, e := range excs {
		if strings.TrimSpace(e.Title) == "" && strings.TrimSpace(e.Detail) == "" {
			continue
		}
		if strings.TrimSpace(e.Fingerprint) == "" {
			e.Fingerprint = e.Title
		}
		out = append(out, e)
	}
	return out
}

func exceptionsFromLines(text string) []scanException {
	var out []scanException
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "===") {
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		title := line
		if len(title) > 100 {
			title = title[:100]
		}
		out = append(out, scanException{
			Title:       title,
			Fingerprint: line,
			Severity:    "medium",
			Detail:      line,
		})
	}
	return out
}
