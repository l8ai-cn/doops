package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReleaseCoordinatorRecoversInterruptedTicketBeforeStarting(t *testing.T) {
	path := t.TempDir() + "/gateway.db"
	store, err := OpenGatewayStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ticket, err := store.CreateReleaseTicket(releaseRequest("interrupted", "prod/api"))
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if _, err := store.ClaimReleaseTicket(ticket.Number); err != nil {
		t.Fatalf("claim ticket: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := OpenGatewayStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	coordinator := newReleaseCoordinator(reopened, &fakeReleaseExecutor{}, time.Hour, time.Minute)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()

	recovered := mustGetRelease(t, reopened, ticket.Number)
	if recovered.Status != ReleaseStatusFailed ||
		recovered.ResultJSON != `{"outcome":"unknown"}` ||
		recovered.Stage != "recovered" {
		t.Fatalf("interrupted ticket was not recovered fail-closed: %#v", recovered)
	}
	if coordinator.Accepting() {
		t.Fatal("coordinator must remain halted after recovering an unknown running release")
	}
}

func TestReleaseCoordinatorStopBeforeStartDoesNotDisableLaterStart(t *testing.T) {
	store := newReleaseTestStore(t)
	coordinator := newReleaseCoordinator(store, &fakeReleaseExecutor{}, time.Hour, time.Minute)

	coordinator.Stop()
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator after an early stop call: %v", err)
	}
	defer coordinator.Stop()
	if !coordinator.Accepting() {
		t.Fatal("an early stop call must not permanently disable coordinator startup")
	}
}

func TestReleaseCoordinatorRunsOneTicketAtATimeWithoutPreemption(t *testing.T) {
	store := newReleaseTestStore(t)
	executor := newFakeReleaseExecutor()
	executor.setOnline("dev", "node", true)
	coordinator := newReleaseCoordinator(store, executor, 5*time.Millisecond, time.Second)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()

	first := releaseRequest("first", "prod/api")
	first.Cluster = "dev"
	first.Instance = "node"
	firstTicket, err := store.CreateReleaseTicket(first)
	if err != nil {
		t.Fatalf("create first ticket: %v", err)
	}
	firstBlock := executor.block(firstTicket.Number)
	coordinator.Wake()
	executor.waitStarted(t, firstTicket.Number)

	second := first
	second.Requirement = "second"
	secondTicket, err := store.CreateReleaseTicket(second)
	if err != nil {
		t.Fatalf("create second ticket: %v", err)
	}
	latest := first
	latest.Requirement = "latest"
	latestTicket, err := store.CreateReleaseTicket(latest)
	if err != nil {
		t.Fatalf("create latest ticket: %v", err)
	}
	other := first
	other.Requirement = "other"
	other.Scope = "prod/worker"
	otherTicket, err := store.CreateReleaseTicket(other)
	if err != nil {
		t.Fatalf("create other ticket: %v", err)
	}
	coordinator.Wake()

	if got := mustGetRelease(t, store, firstTicket.Number); got.Status != ReleaseStatusRunning {
		t.Fatalf("running ticket was preempted: %#v", got)
	}
	if got := mustGetRelease(t, store, secondTicket.Number); got.Status != ReleaseStatusSuperseded {
		t.Fatalf("older queued ticket was not superseded: %#v", got)
	}
	if got := mustGetRelease(t, store, latestTicket.Number); got.Status != ReleaseStatusQueued {
		t.Fatalf("latest queued ticket started before running ticket completed: %#v", got)
	}
	select {
	case number := <-executor.started:
		t.Fatalf("another ticket started while first was running: %d", number)
	case <-time.After(30 * time.Millisecond):
	}

	close(firstBlock)
	waitReleaseStatus(t, store, firstTicket.Number, ReleaseStatusCompleted)
	waitReleaseStatus(t, store, latestTicket.Number, ReleaseStatusCompleted)
	waitReleaseStatus(t, store, otherTicket.Number, ReleaseStatusCompleted)

	if got := executor.startOrder(); fmt.Sprint(got) != fmt.Sprint([]int64{
		firstTicket.Number,
		latestTicket.Number,
		otherTicket.Number,
	}) {
		t.Fatalf("unexpected execution order: %#v", got)
	}
	if executor.maxConcurrent() != 1 {
		t.Fatalf("coordinator executed tickets concurrently: max=%d", executor.maxConcurrent())
	}
}

func TestReleaseCoordinatorLeavesOfflineTicketQueuedUntilTargetReturns(t *testing.T) {
	store := newReleaseTestStore(t)
	executor := newFakeReleaseExecutor()
	coordinator := newReleaseCoordinator(store, executor, 5*time.Millisecond, time.Second)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()

	req := releaseRequest("offline", "prod/api")
	req.Cluster = "dev"
	req.Instance = "node"
	ticket, err := store.CreateReleaseTicket(req)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	coordinator.Wake()
	time.Sleep(30 * time.Millisecond)
	if got := mustGetRelease(t, store, ticket.Number); got.Status != ReleaseStatusQueued || got.StartedAt != nil {
		t.Fatalf("offline ticket must remain unclaimed: %#v", got)
	}

	executor.setOnline("dev", "node", true)
	coordinator.Wake()
	waitReleaseStatus(t, store, ticket.Number, ReleaseStatusCompleted)
}

func TestReleaseCoordinatorDoesNotStarveOnlineTicketBehindOfflineBacklog(t *testing.T) {
	store := newReleaseTestStore(t)
	executor := newFakeReleaseExecutor()
	executor.setOnline("dev", "node", true)

	const offlineCount = 501
	for i := 0; i < offlineCount; i++ {
		req := releaseRequest(fmt.Sprintf("offline %d", i), fmt.Sprintf("offline/scope-%d", i))
		req.Cluster = "offline"
		req.Instance = "node"
		if _, err := store.CreateReleaseTicket(req); err != nil {
			t.Fatalf("create offline ticket %d: %v", i, err)
		}
	}
	req := releaseRequest("online", "prod/api")
	req.Cluster = "dev"
	req.Instance = "node"
	online, err := store.CreateReleaseTicket(req)
	if err != nil {
		t.Fatalf("create online ticket: %v", err)
	}

	coordinator := newReleaseCoordinator(store, executor, 5*time.Millisecond, time.Second)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()

	waitReleaseStatus(t, store, online.Number, ReleaseStatusCompleted)
}

func TestReleaseCoordinatorStopCancelsRunningRelease(t *testing.T) {
	store := newReleaseTestStore(t)
	executor := newFakeReleaseExecutor()
	executor.setOnline("dev", "node", true)
	coordinator := newReleaseCoordinator(store, executor, time.Hour, 2*time.Second)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}

	req := releaseRequest("cancel on stop", "prod/api")
	req.Cluster = "dev"
	req.Instance = "node"
	ticket, err := store.CreateReleaseTicket(req)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	executor.block(ticket.Number)
	coordinator.Wake()
	executor.waitStarted(t, ticket.Number)

	started := time.Now()
	coordinator.Stop()
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("stop waited for the operation timeout instead of canceling execution: %s", elapsed)
	}
	failed := mustGetRelease(t, store, ticket.Number)
	if failed.Status != ReleaseStatusFailed || !strings.Contains(failed.Error, context.Canceled.Error()) {
		t.Fatalf("canceled release must fail with an explicit cancellation result: %#v", failed)
	}
}

func TestReleaseCoordinatorHaltsAfterExecutionOutcomeBecomesUnknown(t *testing.T) {
	store := newReleaseTestStore(t)
	executor := newFakeReleaseExecutor()
	executor.setOnline("dev", "node", true)
	executor.err = errors.New("agent disconnected")
	coordinator := newReleaseCoordinator(store, executor, 5*time.Millisecond, time.Second)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()

	firstReq := releaseRequest("first", "prod/api")
	firstReq.Cluster = "dev"
	firstReq.Instance = "node"
	first, err := store.CreateReleaseTicket(firstReq)
	if err != nil {
		t.Fatalf("create first ticket: %v", err)
	}
	secondReq := releaseRequest("second", "prod/worker")
	secondReq.Cluster = "dev"
	secondReq.Instance = "node"
	second, err := store.CreateReleaseTicket(secondReq)
	if err != nil {
		t.Fatalf("create second ticket: %v", err)
	}
	coordinator.Wake()

	failed := waitReleaseStatus(t, store, first.Number, ReleaseStatusFailed)
	if !strings.Contains(failed.Error, "outcome unknown") {
		t.Fatalf("transport failure must record an unknown execution outcome: %#v", failed)
	}
	time.Sleep(30 * time.Millisecond)
	if got := mustGetRelease(t, store, second.Number); got.Status != ReleaseStatusQueued {
		t.Fatalf("coordinator must not admit another release after an unknown outcome: %#v", got)
	}
	if coordinator.Accepting() {
		t.Fatal("coordinator must reject new release registration after an unknown outcome")
	}
}

func TestReleaseCoordinatorFailsBlockedOrInvalidResultWithoutRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		result string
		err    error
	}{
		{name: "blocked", result: `{"status":"blocked"}`},
		{name: "invalid json", result: `not-json`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newReleaseTestStore(t)
			executor := newFakeReleaseExecutor()
			executor.setOnline("dev", "node", true)
			executor.result = test.result
			executor.err = test.err
			coordinator := newReleaseCoordinator(store, executor, 5*time.Millisecond, time.Second)
			if err := coordinator.Start(); err != nil {
				t.Fatalf("start coordinator: %v", err)
			}
			defer coordinator.Stop()

			req := releaseRequest(test.name, "prod/api")
			req.Cluster = "dev"
			req.Instance = "node"
			ticket, err := store.CreateReleaseTicket(req)
			if err != nil {
				t.Fatalf("create ticket: %v", err)
			}
			coordinator.Wake()
			failed := waitReleaseStatus(t, store, ticket.Number, ReleaseStatusFailed)
			if failed.Error == "" {
				t.Fatalf("failed ticket must explain the terminal error: %#v", failed)
			}
			time.Sleep(20 * time.Millisecond)
			if executor.callsFor(ticket.Number) != 1 {
				t.Fatalf("terminal failure must not be retried: calls=%d", executor.callsFor(ticket.Number))
			}
		})
	}
}

type fakeReleaseExecutor struct {
	mu         sync.Mutex
	online     map[string]bool
	blocks     map[int64]chan struct{}
	calls      map[int64]int
	order      []int64
	active     int
	maxActiveN int
	started    chan int64
	result     string
	err        error
}

func newFakeReleaseExecutor() *fakeReleaseExecutor {
	return &fakeReleaseExecutor{
		online:  make(map[string]bool),
		blocks:  make(map[int64]chan struct{}),
		calls:   make(map[int64]int),
		started: make(chan int64, 32),
		result:  `{"status":"converged"}`,
	}
}

func (f *fakeReleaseExecutor) TargetOnline(cluster, instance string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[cluster+"/"+instance]
}

func (f *fakeReleaseExecutor) ExecuteRelease(ctx context.Context, ticket ReleaseTicket) (string, error) {
	f.mu.Lock()
	f.calls[ticket.Number]++
	f.order = append(f.order, ticket.Number)
	f.active++
	if f.active > f.maxActiveN {
		f.maxActiveN = f.active
	}
	block := f.blocks[ticket.Number]
	result := f.result
	err := f.err
	f.mu.Unlock()

	f.started <- ticket.Number
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}

	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return result, err
}

func (f *fakeReleaseExecutor) setOnline(cluster, instance string, online bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.online == nil {
		f.online = make(map[string]bool)
	}
	f.online[cluster+"/"+instance] = online
}

func (f *fakeReleaseExecutor) block(number int64) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocks == nil {
		f.blocks = make(map[int64]chan struct{})
	}
	block := make(chan struct{})
	f.blocks[number] = block
	return block
}

func (f *fakeReleaseExecutor) waitStarted(t *testing.T, want int64) {
	t.Helper()
	select {
	case got := <-f.started:
		if got != want {
			t.Fatalf("unexpected started ticket %d, want %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("ticket %d did not start", want)
	}
}

func (f *fakeReleaseExecutor) startOrder() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.order...)
}

func (f *fakeReleaseExecutor) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActiveN
}

func (f *fakeReleaseExecutor) callsFor(number int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[number]
}

func waitReleaseStatus(t *testing.T, store *GatewayStore, number int64, want ReleaseStatus) ReleaseTicket {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ticket, err := store.GetReleaseTicket(number)
		if err == nil && ticket.Status == want {
			return ticket
		}
		time.Sleep(5 * time.Millisecond)
	}
	ticket, err := store.GetReleaseTicket(number)
	t.Fatalf("ticket %d did not reach %q: ticket=%#v err=%v", number, want, ticket, err)
	return ReleaseTicket{}
}
