package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type releaseExecutor interface {
	TargetOnline(cluster, instance string) bool
	ExecuteRelease(context.Context, ReleaseTicket) (string, error)
}

var errReleaseCoordinatorHalted = errors.New("release coordinator halted")

const releaseCandidatePageSize = 128

type ReleaseCoordinator struct {
	store            *GatewayStore
	executor         releaseExecutor
	interval         time.Duration
	operationTimeout time.Duration
	wake             chan struct{}
	stop             chan struct{}
	done             chan struct{}
	startMu          sync.Mutex
	started          bool
	stopped          bool
	haltErr          error
	stopOnce         sync.Once
	ctx              context.Context
	cancel           context.CancelFunc
}

func NewReleaseCoordinator(hub *GatewayHub, store *GatewayStore, interval time.Duration) *ReleaseCoordinator {
	return newReleaseCoordinator(store, gatewayReleaseExecutor{hub: hub}, interval, hub.opts.OperationTimeout)
}

func newReleaseCoordinator(
	store *GatewayStore,
	executor releaseExecutor,
	interval time.Duration,
	operationTimeout time.Duration,
) *ReleaseCoordinator {
	if interval <= 0 {
		interval = time.Second
	}
	if operationTimeout <= 0 {
		operationTimeout = 30 * time.Minute
	}
	return &ReleaseCoordinator{
		store:            store,
		executor:         executor,
		interval:         interval,
		operationTimeout: operationTimeout,
		wake:             make(chan struct{}, 1),
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
	}
}

func (c *ReleaseCoordinator) Start() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return nil
	}
	if c.store == nil || c.executor == nil {
		return fmt.Errorf("release coordinator requires store and executor")
	}
	recovered, err := c.store.RecoverInterruptedReleases()
	if err != nil {
		return fmt.Errorf("recover interrupted releases: %w", err)
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.started = true
	if recovered > 0 {
		c.haltErr = fmt.Errorf("%w: recovered %d interrupted release ticket(s) with outcome unknown",
			errReleaseCoordinatorHalted, recovered)
		close(c.done)
		log.Printf("[cicd] %v", c.haltErr)
		return nil
	}
	go c.loop()
	c.Wake()
	return nil
}

func (c *ReleaseCoordinator) Stop() {
	c.startMu.Lock()
	started := c.started
	cancel := c.cancel
	if started {
		c.stopped = true
	}
	c.startMu.Unlock()
	if !started {
		return
	}
	c.stopOnce.Do(func() {
		cancel()
		close(c.stop)
	})
	<-c.done
}

func (c *ReleaseCoordinator) Accepting() bool {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return c.started && !c.stopped && c.haltErr == nil
}

func (c *ReleaseCoordinator) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *ReleaseCoordinator) loop() {
	defer close(c.done)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	log.Printf("[cicd] release coordinator started (tick=%s)", c.interval)
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		processed, err := c.processNext()
		if err != nil {
			if errors.Is(err, errReleaseCoordinatorHalted) {
				c.startMu.Lock()
				c.haltErr = err
				c.startMu.Unlock()
				log.Printf("[cicd] %v", err)
				return
			}
			log.Printf("[cicd] release coordinator error: %v", err)
		}
		if processed {
			continue
		}
		select {
		case <-c.wake:
		case <-ticker.C:
		case <-c.stop:
			return
		}
	}
}

func (c *ReleaseCoordinator) processNext() (bool, error) {
	var afterNumber int64
	for {
		tickets, err := c.store.listQueuedReleaseCandidates(afterNumber, releaseCandidatePageSize)
		if err != nil {
			return false, err
		}
		if len(tickets) == 0 {
			return false, nil
		}
		for _, queued := range tickets {
			if !c.executor.TargetOnline(queued.Cluster, queued.Instance) {
				continue
			}
			ticket, err := c.store.ClaimReleaseTicket(queued.Number)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return false, fmt.Errorf("claim release %d: %w", queued.Number, err)
			}
			if err := c.store.UpdateReleaseProgress(ticket.Number, "executing", "agent execution started"); err != nil {
				_, failErr := c.store.FailReleaseTicket(ticket.Number, "failed to record execution progress: "+err.Error(), "")
				if failErr != nil {
					return true, haltReleaseCoordinator(fmt.Errorf(
						"record and fail release %d: progress=%v fail=%w", ticket.Number, err, failErr))
				}
				return true, nil
			}

			ctx, cancel := context.WithTimeout(c.ctx, c.operationTimeout)
			resultJSON, runErr := c.executor.ExecuteRelease(ctx, ticket)
			cancel()
			resultJSON = strings.TrimSpace(resultJSON)
			if runErr != nil {
				const outcomeUnknown = `{"outcome":"unknown"}`
				message := "execution outcome unknown: " + runErr.Error()
				if _, err := c.store.FailReleaseTicket(ticket.Number, message, outcomeUnknown); err != nil {
					return true, haltReleaseCoordinator(fmt.Errorf(
						"fail release %d after execution transport error: %w", ticket.Number, err))
				}
				return true, haltReleaseCoordinator(fmt.Errorf(
					"release %d %s", ticket.Number, message))
			}
			if err := c.store.UpdateReleaseProgress(ticket.Number, "validating", "validating reconciliation result"); err != nil {
				if _, failErr := c.store.FailReleaseTicket(ticket.Number, "failed to record validation progress: "+err.Error(), resultJSON); failErr != nil {
					return true, haltReleaseCoordinator(fmt.Errorf(
						"record and fail release %d: progress=%v fail=%w", ticket.Number, err, failErr))
				}
				return true, nil
			}

			status, err := releaseResultStatus(resultJSON)
			if err != nil {
				if _, failErr := c.store.FailReleaseTicket(ticket.Number, err.Error(), resultJSON); failErr != nil {
					return true, haltReleaseCoordinator(fmt.Errorf(
						"fail release %d with invalid result: %w", ticket.Number, failErr))
				}
				return true, nil
			}
			if status != "converged" {
				message := fmt.Sprintf("reconciliation finished with status %s", status)
				if _, err := c.store.FailReleaseTicket(ticket.Number, message, resultJSON); err != nil {
					return true, haltReleaseCoordinator(fmt.Errorf(
						"fail release %d with status %s: %w", ticket.Number, status, err))
				}
				return true, nil
			}
			if _, err := c.store.CompleteReleaseTicket(ticket.Number, resultJSON); err != nil {
				return true, haltReleaseCoordinator(fmt.Errorf("complete release %d: %w", ticket.Number, err))
			}
			return true, nil
		}
		afterNumber = tickets[len(tickets)-1].Number
	}
}

func haltReleaseCoordinator(err error) error {
	return fmt.Errorf("%w: %v", errReleaseCoordinatorHalted, err)
}

func releaseResultStatus(resultJSON string) (string, error) {
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return "", fmt.Errorf("reconciliation result is not one JSON object: %w", err)
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	switch status {
	case "converged", "blocked", "failed":
		return status, nil
	default:
		return "", fmt.Errorf("unsupported reconciliation result status %q", status)
	}
}

type gatewayReleaseExecutor struct {
	hub *GatewayHub
}

func (e gatewayReleaseExecutor) TargetOnline(cluster, instance string) bool {
	return e.hub != nil && e.hub.getAgent(cluster, instance) != nil
}

func (e gatewayReleaseExecutor) ExecuteRelease(ctx context.Context, ticket ReleaseTicket) (string, error) {
	var requiredEvidence []string
	if err := json.Unmarshal([]byte(ticket.RequiredEvidenceJSON), &requiredEvidence); err != nil {
		return "", fmt.Errorf("decode required evidence contract: %w", err)
	}
	return e.hub.RunInternalToolCallFinal(ctx, ticket.Cluster, ticket.Instance, "doops_agent_prompt", map[string]interface{}{
		"session_id":        ticket.SessionID,
		"instruction":       ticket.Instruction,
		"operation":         "reconcile",
		"plan_digest":       ticket.PlanDigest,
		"execution_mode":    ticket.ExecutionMode,
		"source_revision":   ticket.SourceRevision,
		"workspace_commit":  ticket.WorkspaceCommit,
		"required_evidence": requiredEvidence,
		"response_format":   "json",
	})
}
