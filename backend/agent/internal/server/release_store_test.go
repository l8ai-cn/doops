package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestReleaseTicketsUseIncrementingNumbers(t *testing.T) {
	store := newReleaseTestStore(t)

	first, err := store.CreateReleaseTicket(releaseRequest("deploy api", "scope-a"))
	if err != nil {
		t.Fatalf("create first ticket: %v", err)
	}
	second, err := store.CreateReleaseTicket(releaseRequest("deploy web", "scope-b"))
	if err != nil {
		t.Fatalf("create second ticket: %v", err)
	}

	if first.Number <= 0 {
		t.Fatalf("expected positive release number, got %d", first.Number)
	}
	if second.Number != first.Number+1 {
		t.Fatalf("expected incrementing release numbers, got %d then %d", first.Number, second.Number)
	}
}

func TestReleaseTicketsLatestWinsSupersedesOnlyQueuedInSameScope(t *testing.T) {
	store := newReleaseTestStore(t)

	running, err := store.CreateReleaseTicket(releaseRequest("running release", "prod/api"))
	if err != nil {
		t.Fatalf("create running ticket: %v", err)
	}
	if _, err := store.ClaimReleaseTicket(running.Number); err != nil {
		t.Fatalf("claim running ticket: %v", err)
	}
	oldQueued, err := store.CreateReleaseTicket(releaseRequest("old queued release", "prod/api"))
	if err != nil {
		t.Fatalf("create old queued ticket: %v", err)
	}
	otherScope, err := store.CreateReleaseTicket(releaseRequest("other queued release", "prod/web"))
	if err != nil {
		t.Fatalf("create other queued ticket: %v", err)
	}

	latest, err := store.CreateReleaseTicket(releaseRequest("latest release", "prod/api"))
	if err != nil {
		t.Fatalf("create latest ticket: %v", err)
	}

	gotRunning := mustGetRelease(t, store, running.Number)
	if gotRunning.Status != ReleaseStatusRunning {
		t.Fatalf("running ticket must not be superseded, got %q", gotRunning.Status)
	}
	gotOldQueued := mustGetRelease(t, store, oldQueued.Number)
	if gotOldQueued.Status != ReleaseStatusSuperseded || gotOldQueued.SupersededBy == nil || *gotOldQueued.SupersededBy != latest.Number {
		t.Fatalf("old queued ticket was not superseded by latest: %#v", gotOldQueued)
	}
	gotOtherScope := mustGetRelease(t, store, otherScope.Number)
	if gotOtherScope.Status != ReleaseStatusQueued {
		t.Fatalf("other scope queued ticket must remain queued, got %q", gotOtherScope.Status)
	}

	queued, err := store.ListQueuedReleaseTickets(10)
	if err != nil {
		t.Fatalf("list queued tickets: %v", err)
	}
	if len(queued) != 2 || !releaseListHasNumber(queued, otherScope.Number) || !releaseListHasNumber(queued, latest.Number) {
		t.Fatalf("expected other scope and latest same-scope tickets to be queued, got %#v", queued)
	}
	if !hasReleaseEvent(gotOldQueued, ReleaseEventSuperseded) {
		t.Fatalf("superseded ticket should expose superseded event: %#v", gotOldQueued.Events)
	}
}

func TestConcurrentReleaseTicketsLeaveOnlyLatestQueuedInScope(t *testing.T) {
	store := newReleaseTestStore(t)

	const count = 12
	var wg sync.WaitGroup
	numbers := make(chan int64, count)
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := releaseRequest(fmt.Sprintf("concurrent release %d", index), "prod/concurrent")
			req.SessionID = fmt.Sprintf("session-%d", index)
			ticket, err := store.CreateReleaseTicket(req)
			if err != nil {
				errs <- err
				return
			}
			numbers <- ticket.Number
		}(i)
	}
	wg.Wait()
	close(numbers)
	close(errs)

	for err := range errs {
		t.Fatalf("create concurrent release: %v", err)
	}
	seen := make(map[int64]bool, count)
	var latest int64
	for number := range numbers {
		if seen[number] {
			t.Fatalf("duplicate release number %d", number)
		}
		seen[number] = true
		if number > latest {
			latest = number
		}
	}
	if len(seen) != count {
		t.Fatalf("expected %d release numbers, got %d", count, len(seen))
	}

	queued, err := store.ListQueuedReleaseTickets(count)
	if err != nil {
		t.Fatalf("list queued releases: %v", err)
	}
	if len(queued) != 1 || queued[0].Number != latest {
		t.Fatalf("only latest release %d should remain queued, got %#v", latest, queued)
	}
	for number := range seen {
		ticket := mustGetRelease(t, store, number)
		if ticket.UpdatedAt.Before(ticket.CreatedAt) {
			t.Fatalf("release %d updated before it was created: %#v", number, ticket)
		}
		for i := 1; i < len(ticket.Events); i++ {
			if ticket.Events[i].CreatedAt.Before(ticket.Events[i-1].CreatedAt) {
				t.Fatalf("release %d events are not chronological: %#v", number, ticket.Events)
			}
		}
		if number == latest {
			if ticket.Status != ReleaseStatusQueued {
				t.Fatalf("latest release %d must remain queued, got %q", number, ticket.Status)
			}
			continue
		}
		for hops := 0; hops < count; hops++ {
			if ticket.Status != ReleaseStatusSuperseded || ticket.SupersededBy == nil || *ticket.SupersededBy <= ticket.Number {
				t.Fatalf("release %d has an invalid supersession chain: %#v", number, ticket)
			}
			ticket = mustGetRelease(t, store, *ticket.SupersededBy)
			if ticket.Number == latest {
				if ticket.Status != ReleaseStatusQueued {
					t.Fatalf("supersession chain for %d ended at non-queued latest ticket: %#v", number, ticket)
				}
				break
			}
			if hops == count-1 {
				t.Fatalf("supersession chain for %d did not reach latest release %d", number, latest)
			}
		}
	}
}

func TestListQueuedReleaseTicketsWithoutLimitReturnsEveryQueuedScope(t *testing.T) {
	store := newReleaseTestStore(t)

	const count = 505
	for i := 0; i < count; i++ {
		if _, err := store.CreateReleaseTicket(releaseRequest(
			fmt.Sprintf("release %d", i),
			fmt.Sprintf("prod/scope-%d", i),
		)); err != nil {
			t.Fatalf("create release %d: %v", i, err)
		}
	}

	queued, err := store.ListQueuedReleaseTickets(0)
	if err != nil {
		t.Fatalf("list all queued releases: %v", err)
	}
	if len(queued) != count {
		t.Fatalf("expected all %d queued releases, got %d", count, len(queued))
	}
}

func TestReleaseTicketJSONExposesQueryableStatusRequirementAndEvents(t *testing.T) {
	store := newReleaseTestStore(t)

	queued, err := store.CreateReleaseTicket(releaseRequest("ship payment fix", "prod/payments"))
	if err != nil {
		t.Fatalf("create queued ticket: %v", err)
	}
	running, err := store.ClaimReleaseTicket(queued.Number)
	if err != nil {
		t.Fatalf("claim queued ticket: %v", err)
	}
	if err := store.UpdateReleaseProgress(running.Number, "planning", "plan accepted"); err != nil {
		t.Fatalf("update progress: %v", err)
	}
	completed, err := store.CompleteReleaseTicket(running.Number, `{"health":"ok"}`)
	if err != nil {
		t.Fatalf("complete ticket: %v", err)
	}
	failed, err := store.CreateReleaseTicket(releaseRequest("ship worker fix", "prod/worker"))
	if err != nil {
		t.Fatalf("create failed candidate: %v", err)
	}
	failed, err = store.FailReleaseTicket(failed.Number, "precheck failed", `{"precheck":"failed"}`)
	if err != nil {
		t.Fatalf("fail ticket: %v", err)
	}
	superseded, err := store.CreateReleaseTicket(releaseRequest("old config", "prod/config"))
	if err != nil {
		t.Fatalf("create superseded candidate: %v", err)
	}
	if _, err := store.CreateReleaseTicket(releaseRequest("new config", "prod/config")); err != nil {
		t.Fatalf("create superseding ticket: %v", err)
	}
	superseded = mustGetRelease(t, store, superseded.Number)

	for _, ticket := range []ReleaseTicket{completed, failed, superseded} {
		got := mustGetRelease(t, store, ticket.Number)
		if got.Requirement == "" || got.UserID == "" || got.SessionID == "" {
			t.Fatalf("ticket should expose requirement and original user/session: %#v", got)
		}
		if got.Cluster == "" || got.Instance == "" || got.Scope == "" || got.Application == "" || got.Environment == "" || got.Namespace == "" || got.ReleaseName == "" {
			t.Fatalf("ticket should expose target fields: %#v", got)
		}
		if got.PlanDigest == "" || got.SourceRevision == "" || got.WorkspaceCommit == "" {
			t.Fatalf("ticket should expose plan/source/snapshot identifiers: %#v", got)
		}
		if got.SourceRevision == got.WorkspaceCommit {
			t.Fatalf("source revision and snapshot commit must remain distinct: %#v", got)
		}
		if got.Instruction != "run privileged deployment steps" {
			t.Fatalf("internal release execution must retain its instruction: %#v", got)
		}
		if len(got.Events) == 0 {
			t.Fatalf("ticket should expose events: %#v", got)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal ticket %d: %v", got.Number, err)
		}
		text := string(raw)
		if strings.Contains(text, "run privileged deployment steps") ||
			strings.Contains(text, "instruction") ||
			strings.Contains(text, "tok_123") ||
			strings.Contains(text, "token_id") {
			t.Fatalf("release ticket JSON must not expose instruction or token identity: %s", text)
		}
		if !strings.Contains(text, string(got.Status)) {
			t.Fatalf("release ticket JSON should include status %q: %s", got.Status, text)
		}
	}
}

func TestClaimReleaseTicketIsAtomicQueuedToRunning(t *testing.T) {
	store := newReleaseTestStore(t)

	ticket, err := store.CreateReleaseTicket(releaseRequest("deploy once", "prod/api"))
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	claimed, err := store.ClaimReleaseTicket(ticket.Number)
	if err != nil {
		t.Fatalf("first claim should succeed: %v", err)
	}
	if claimed.Status != ReleaseStatusRunning || claimed.StartedAt == nil {
		t.Fatalf("claim should mark ticket running with start time: %#v", claimed)
	}
	if _, err := store.ClaimReleaseTicket(ticket.Number); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second claim should fail with sql.ErrNoRows, got %v", err)
	}
	got := mustGetRelease(t, store, ticket.Number)
	if got.Status != ReleaseStatusRunning {
		t.Fatalf("failed second claim must not change status, got %q", got.Status)
	}
}

func TestClaimReleaseTicketRejectsAnySecondRunningRelease(t *testing.T) {
	store := newReleaseTestStore(t)

	running, err := store.CreateReleaseTicket(releaseRequest("running", "prod/api"))
	if err != nil {
		t.Fatalf("create running ticket: %v", err)
	}
	if _, err := store.ClaimReleaseTicket(running.Number); err != nil {
		t.Fatalf("claim running ticket: %v", err)
	}
	queued, err := store.CreateReleaseTicket(releaseRequest("queued", "prod/worker"))
	if err != nil {
		t.Fatalf("create queued ticket: %v", err)
	}

	if _, err := store.ClaimReleaseTicket(queued.Number); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second running release must be rejected across scopes, got %v", err)
	}
	got := mustGetRelease(t, store, queued.Number)
	if got.Status != ReleaseStatusQueued || got.StartedAt != nil {
		t.Fatalf("rejected claim changed queued ticket: %#v", got)
	}
	if _, err := store.CompleteReleaseTicket(running.Number, `{"status":"ok"}`); err != nil {
		t.Fatalf("complete running ticket: %v", err)
	}
	claimed, err := store.ClaimReleaseTicket(queued.Number)
	if err != nil {
		t.Fatalf("claim queued ticket after scope is released: %v", err)
	}
	if claimed.Status != ReleaseStatusRunning || claimed.StartedAt == nil {
		t.Fatalf("queued ticket did not start after scope was released: %#v", claimed)
	}
}

func TestListQueuedReleaseCandidatesPaginatesWithoutLoadingEvents(t *testing.T) {
	store := newReleaseTestStore(t)

	for i := 0; i < 5; i++ {
		if _, err := store.CreateReleaseTicket(releaseRequest(
			fmt.Sprintf("release %d", i),
			fmt.Sprintf("prod/page-%d", i),
		)); err != nil {
			t.Fatalf("create release %d: %v", i, err)
		}
	}

	first, err := store.listQueuedReleaseCandidates(0, 2)
	if err != nil {
		t.Fatalf("list first candidate page: %v", err)
	}
	if len(first) != 2 || len(first[0].Events) != 0 || len(first[1].Events) != 0 {
		t.Fatalf("candidate page must contain two tickets without event timelines: %#v", first)
	}
	second, err := store.listQueuedReleaseCandidates(first[len(first)-1].Number, 2)
	if err != nil {
		t.Fatalf("list second candidate page: %v", err)
	}
	if len(second) != 2 || second[0].Number <= first[len(first)-1].Number {
		t.Fatalf("candidate pagination did not advance: first=%#v second=%#v", first, second)
	}
	last, err := store.listQueuedReleaseCandidates(second[len(second)-1].Number, 2)
	if err != nil {
		t.Fatalf("list last candidate page: %v", err)
	}
	if len(last) != 1 || last[0].Number <= second[len(second)-1].Number {
		t.Fatalf("unexpected last candidate page: %#v", last)
	}
}

func TestUpdateReleaseProgressRequiresRunningTicket(t *testing.T) {
	store := newReleaseTestStore(t)

	queued, err := store.CreateReleaseTicket(releaseRequest("queued", "prod/queued"))
	if err != nil {
		t.Fatalf("create queued ticket: %v", err)
	}
	completed, err := store.CreateReleaseTicket(releaseRequest("completed", "prod/completed"))
	if err != nil {
		t.Fatalf("create completed ticket: %v", err)
	}
	if _, err := store.ClaimReleaseTicket(completed.Number); err != nil {
		t.Fatalf("claim completed candidate: %v", err)
	}
	completed, err = store.CompleteReleaseTicket(completed.Number, `{"status":"ok"}`)
	if err != nil {
		t.Fatalf("complete ticket: %v", err)
	}
	failed, err := store.CreateReleaseTicket(releaseRequest("failed", "prod/failed"))
	if err != nil {
		t.Fatalf("create failed ticket: %v", err)
	}
	failed, err = store.FailReleaseTicket(failed.Number, "failed", "")
	if err != nil {
		t.Fatalf("fail ticket: %v", err)
	}
	superseded, err := store.CreateReleaseTicket(releaseRequest("superseded", "prod/superseded"))
	if err != nil {
		t.Fatalf("create superseded ticket: %v", err)
	}
	if _, err := store.CreateReleaseTicket(releaseRequest("latest", "prod/superseded")); err != nil {
		t.Fatalf("supersede ticket: %v", err)
	}
	superseded = mustGetRelease(t, store, superseded.Number)

	for _, ticket := range []ReleaseTicket{queued, completed, failed, superseded} {
		before := mustGetRelease(t, store, ticket.Number)
		if err := store.UpdateReleaseProgress(ticket.Number, "late", "must be rejected"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("ticket %d status %q accepted progress: %v", ticket.Number, ticket.Status, err)
		}
		after := mustGetRelease(t, store, ticket.Number)
		if after.Stage != before.Stage || len(after.Events) != len(before.Events) {
			t.Fatalf("rejected progress changed ticket %d: before=%#v after=%#v", ticket.Number, before, after)
		}
	}
}

func TestConcurrentReleaseProgressKeepsChronologicalTimeline(t *testing.T) {
	store := newReleaseTestStore(t)
	ticket, err := store.CreateReleaseTicket(releaseRequest("progress timeline", "prod/progress"))
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if _, err := store.ClaimReleaseTicket(ticket.Number); err != nil {
		t.Fatalf("claim ticket: %v", err)
	}

	const count = 16
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs <- store.UpdateReleaseProgress(
				ticket.Number,
				fmt.Sprintf("stage-%d", index),
				fmt.Sprintf("progress-%d", index),
			)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("update concurrent progress: %v", err)
		}
	}

	got := mustGetRelease(t, store, ticket.Number)
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("release updated before it was created: %#v", got)
	}
	for i := 1; i < len(got.Events); i++ {
		if got.Events[i].CreatedAt.Before(got.Events[i-1].CreatedAt) {
			t.Fatalf("release events are not chronological: %#v", got.Events)
		}
	}
}

func TestReleaseTicketRejectsCorruptStoredTime(t *testing.T) {
	store := newReleaseTestStore(t)
	ticket, err := store.CreateReleaseTicket(releaseRequest("corrupt time", "prod/time"))
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE release_tickets SET created_at = ? WHERE number = ?`, "not-a-time", ticket.Number); err != nil {
		t.Fatalf("corrupt stored time: %v", err)
	}
	if _, err := store.GetReleaseTicket(ticket.Number); err == nil {
		t.Fatal("corrupt release timestamp must not be returned as a zero time")
	}
}

func TestReleaseEventsEnforceTicketForeignKey(t *testing.T) {
	store := newReleaseTestStore(t)
	_, err := store.db.Exec(`INSERT INTO release_events
		(release_number, type, status, stage, message, data_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		int64(999999), string(ReleaseEventCreated), string(ReleaseStatusQueued), "", "", "", "2026-07-14T00:00:00Z")
	if err == nil {
		t.Fatal("orphan release event must be rejected")
	}
}

func TestRecoverInterruptedReleasesMarksRunningFailedWithUnknownOutcome(t *testing.T) {
	store := newReleaseTestStore(t)

	running, err := store.CreateReleaseTicket(releaseRequest("deploy interrupted", "prod/api"))
	if err != nil {
		t.Fatalf("create running candidate: %v", err)
	}
	if _, err := store.ClaimReleaseTicket(running.Number); err != nil {
		t.Fatalf("claim running candidate: %v", err)
	}
	queued, err := store.CreateReleaseTicket(releaseRequest("still queued", "prod/worker"))
	if err != nil {
		t.Fatalf("create queued ticket: %v", err)
	}

	recovered, err := store.RecoverInterruptedReleases()
	if err != nil {
		t.Fatalf("recover interrupted releases: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected one interrupted release recovered, got %d", recovered)
	}

	gotRunning := mustGetRelease(t, store, running.Number)
	if gotRunning.Status != ReleaseStatusFailed || gotRunning.FinishedAt == nil {
		t.Fatalf("running release should be failed by recovery: %#v", gotRunning)
	}
	if !strings.Contains(gotRunning.Error, "outcome unknown") {
		t.Fatalf("recovery error should explicitly say outcome unknown, got %q", gotRunning.Error)
	}
	if !hasReleaseEvent(gotRunning, ReleaseEventRecovered) {
		t.Fatalf("recovered ticket should expose recovery event: %#v", gotRunning.Events)
	}
	gotQueued := mustGetRelease(t, store, queued.Number)
	if gotQueued.Status != ReleaseStatusQueued {
		t.Fatalf("queued release must not be recovered, got %q", gotQueued.Status)
	}
}

func newReleaseTestStore(t *testing.T) *GatewayStore {
	t.Helper()
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func releaseRequest(requirement, scope string) CreateReleaseTicketRequest {
	return CreateReleaseTicketRequest{
		UserID:               "usr_123",
		TokenID:              "tok_123",
		SessionID:            "session-123",
		Requirement:          requirement,
		Cluster:              "prod",
		Instance:             "node-1",
		Scope:                scope,
		Application:          "billing",
		Environment:          "production",
		Namespace:            "billing-prod",
		ReleaseName:          "billing-api",
		PlanDigest:           "plan:abcdef",
		SourceRevision:       "242f51a42db38a2364009898f63d879248048f1d",
		WorkspaceCommit:      "deadbeef",
		ExecutionMode:        "agent",
		Instruction:          "run privileged deployment steps",
		RequiredEvidenceJSON: `["health","logs"]`,
	}
}

func mustGetRelease(t *testing.T, store *GatewayStore, number int64) ReleaseTicket {
	t.Helper()
	ticket, err := store.GetReleaseTicket(number)
	if err != nil {
		t.Fatalf("get release %d: %v", number, err)
	}
	return ticket
}

func hasReleaseEvent(ticket ReleaseTicket, eventType ReleaseEventType) bool {
	for _, event := range ticket.Events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func releaseListHasNumber(tickets []ReleaseTicket, number int64) bool {
	for _, ticket := range tickets {
		if ticket.Number == number {
			return true
		}
	}
	return false
}
