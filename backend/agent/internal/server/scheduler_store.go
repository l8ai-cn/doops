package server

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SchedulerJob 描述一个定时巡检任务：周期性扫描匹配实例的运行时异常，
// 归一化后到对应 git 平台提 issue。
type SchedulerJob struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ClusterGlob    string    `json:"cluster_glob"`
	InstanceGlob   string    `json:"instance_glob"`
	IntervalSec    int64     `json:"interval_sec"`
	ScanMode       string    `json:"scan_mode"` // ask | exec | audit
	ScanConfig     string    `json:"scan_config"`
	Platform       string    `json:"platform"` // cnb | github
	RepoSlug       string    `json:"repo_slug"`
	Labels         string    `json:"labels"`
	TokenEnv       string    `json:"token_env"`
	APIBase        string    `json:"api_base"`
	DedupWindowSec int64     `json:"dedup_window_sec"`
	Enabled        bool      `json:"enabled"`
	LastRunAt      time.Time `json:"last_run_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// SchedulerIssue 是一次已提交 issue 的去重记录。
type SchedulerIssue struct {
	ID          int64     `json:"id"`
	JobID       string    `json:"job_id"`
	Fingerprint string    `json:"fingerprint"`
	RepoSlug    string    `json:"repo_slug"`
	Cluster     string    `json:"cluster"`
	Instance    string    `json:"instance"`
	IssueURL    string    `json:"issue_url"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *GatewayStore) CreateSchedulerJob(job SchedulerJob) (SchedulerJob, error) {
	job.Name = strings.TrimSpace(job.Name)
	if job.Name == "" {
		return SchedulerJob{}, fmt.Errorf("job name is required")
	}
	if job.IntervalSec <= 0 {
		job.IntervalSec = 3600
	}
	if strings.TrimSpace(job.ClusterGlob) == "" {
		job.ClusterGlob = "*"
	}
	if strings.TrimSpace(job.InstanceGlob) == "" {
		job.InstanceGlob = "*"
	}
	if strings.TrimSpace(job.ScanMode) == "" {
		job.ScanMode = "ask"
	}
	if strings.TrimSpace(job.Platform) == "" {
		job.Platform = "cnb"
	}
	if strings.TrimSpace(job.ScanConfig) == "" {
		job.ScanConfig = "{}"
	}
	if job.DedupWindowSec <= 0 {
		job.DedupWindowSec = 86400
	}
	job.ID = "job_" + randomHex(10)
	job.CreatedAt = time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO scheduler_jobs
		(id, name, cluster_glob, instance_glob, interval_sec, scan_mode, scan_config,
		 platform, repo_slug, labels, token_env, api_base, dedup_window_sec, enabled, last_run_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.ClusterGlob, job.InstanceGlob, job.IntervalSec, job.ScanMode, job.ScanConfig,
		job.Platform, job.RepoSlug, job.Labels, job.TokenEnv, job.APIBase, job.DedupWindowSec,
		boolToInt(job.Enabled), "", formatTime(job.CreatedAt))
	if err != nil {
		return SchedulerJob{}, err
	}
	return job, nil
}

func (s *GatewayStore) ListSchedulerJobs() ([]SchedulerJob, error) {
	rows, err := s.db.Query(`SELECT id, name, cluster_glob, instance_glob, interval_sec, scan_mode, scan_config,
		platform, repo_slug, labels, token_env, api_base, dedup_window_sec, enabled, last_run_at, created_at
		FROM scheduler_jobs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []SchedulerJob
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *GatewayStore) GetSchedulerJob(id string) (SchedulerJob, error) {
	row := s.db.QueryRow(`SELECT id, name, cluster_glob, instance_glob, interval_sec, scan_mode, scan_config,
		platform, repo_slug, labels, token_env, api_base, dedup_window_sec, enabled, last_run_at, created_at
		FROM scheduler_jobs WHERE id = ?`, strings.TrimSpace(id))
	return scanJob(row)
}

func (s *GatewayStore) DeleteSchedulerJob(id string) error {
	res, err := s.db.Exec(`DELETE FROM scheduler_jobs WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *GatewayStore) SetSchedulerJobEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE scheduler_jobs SET enabled = ? WHERE id = ?`, boolToInt(enabled), strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *GatewayStore) TouchSchedulerJobRun(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE scheduler_jobs SET last_run_at = ? WHERE id = ?`, formatTime(at.UTC()), strings.TrimSpace(id))
	return err
}

// DueSchedulerJobs 返回当前到点应执行的启用任务。
func (s *GatewayStore) DueSchedulerJobs(now time.Time) ([]SchedulerJob, error) {
	jobs, err := s.ListSchedulerJobs()
	if err != nil {
		return nil, err
	}
	due := jobs[:0]
	for _, job := range jobs {
		if !job.Enabled {
			continue
		}
		if job.LastRunAt.IsZero() || now.Sub(job.LastRunAt) >= time.Duration(job.IntervalSec)*time.Second {
			due = append(due, job)
		}
	}
	return due, nil
}

// FindRecentIssue 判断去重窗口内是否已对同一 fingerprint 提过 issue。
func (s *GatewayStore) FindRecentIssue(repoSlug, fingerprint string, since time.Time) (SchedulerIssue, bool, error) {
	row := s.db.QueryRow(`SELECT id, job_id, fingerprint, repo_slug, cluster, instance, issue_url, title, status, created_at
		FROM scheduler_issues
		WHERE repo_slug = ? AND fingerprint = ? AND created_at >= ?
		ORDER BY created_at DESC LIMIT 1`,
		repoSlug, fingerprint, formatTime(since.UTC()))
	issue, err := scanIssue(row)
	if err == sql.ErrNoRows {
		return SchedulerIssue{}, false, nil
	}
	if err != nil {
		return SchedulerIssue{}, false, err
	}
	return issue, true, nil
}

func (s *GatewayStore) RecordIssue(issue SchedulerIssue) (SchedulerIssue, error) {
	issue.CreatedAt = time.Now().UTC()
	if strings.TrimSpace(issue.Status) == "" {
		issue.Status = "created"
	}
	res, err := s.db.Exec(`INSERT INTO scheduler_issues
		(job_id, fingerprint, repo_slug, cluster, instance, issue_url, title, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issue.JobID, issue.Fingerprint, issue.RepoSlug, issue.Cluster, issue.Instance,
		issue.IssueURL, issue.Title, issue.Status, formatTime(issue.CreatedAt))
	if err != nil {
		return SchedulerIssue{}, err
	}
	issue.ID, _ = res.LastInsertId()
	return issue, nil
}

func (s *GatewayStore) ListSchedulerIssues(jobID string, limit int) ([]SchedulerIssue, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id, job_id, fingerprint, repo_slug, cluster, instance, issue_url, title, status, created_at
		FROM scheduler_issues`
	args := []interface{}{}
	if strings.TrimSpace(jobID) != "" {
		query += ` WHERE job_id = ?`
		args = append(args, strings.TrimSpace(jobID))
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var issues []SchedulerIssue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanJob(row rowScanner) (SchedulerJob, error) {
	var job SchedulerJob
	var enabled int
	var lastRun, created string
	err := row.Scan(&job.ID, &job.Name, &job.ClusterGlob, &job.InstanceGlob, &job.IntervalSec,
		&job.ScanMode, &job.ScanConfig, &job.Platform, &job.RepoSlug, &job.Labels, &job.TokenEnv,
		&job.APIBase, &job.DedupWindowSec, &enabled, &lastRun, &created)
	if err != nil {
		return SchedulerJob{}, err
	}
	job.Enabled = enabled != 0
	if lastRun != "" {
		job.LastRunAt, _ = parseTime(lastRun)
	}
	job.CreatedAt, _ = parseTime(created)
	return job, nil
}

func scanIssue(row rowScanner) (SchedulerIssue, error) {
	var issue SchedulerIssue
	var created string
	err := row.Scan(&issue.ID, &issue.JobID, &issue.Fingerprint, &issue.RepoSlug, &issue.Cluster,
		&issue.Instance, &issue.IssueURL, &issue.Title, &issue.Status, &created)
	if err != nil {
		return SchedulerIssue{}, err
	}
	issue.CreatedAt, _ = parseTime(created)
	return issue, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
