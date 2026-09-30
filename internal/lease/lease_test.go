package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/signal"
)

type clock struct{ t time.Time }

func newClock() *clock                   { return &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)} }
func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestAssignRefusesActiveLease(t *testing.T) {
	reg := NewRegistry()
	clk := newClock()
	vessel := uuid.New()

	first, err := reg.Assign(vessel, "A", time.Minute, clk.now())
	if err != nil {
		t.Fatalf("initial assign: %v", err)
	}
	if first.FencingToken != 1 || first.Owner != "A" {
		t.Fatalf("unexpected initial lease: %+v", first)
	}

	// A still holds an unexpired control lease: B must not be able to grab it.
	if _, err := reg.Assign(vessel, "B", time.Minute, clk.now()); !errors.Is(err, ErrLeaseStillActive) {
		t.Fatalf("assign over active lease = %v, want ErrLeaseStillActive", err)
	}
	if current, ok := reg.Get(vessel); !ok || current.Owner != "A" || current.FencingToken != 1 {
		t.Fatalf("active lease changed after rejected assign: %+v ok=%v", current, ok)
	}

	// Once expired the slot is free again and the fencing token advances.
	clk.advance(61 * time.Second)
	reassigned, err := reg.Assign(vessel, "B", time.Minute, clk.now())
	if err != nil {
		t.Fatalf("assign after expiry: %v", err)
	}
	if reassigned.Owner != "B" || reassigned.FencingToken != 2 {
		t.Fatalf("unexpected post-expiry lease: %+v", reassigned)
	}
}

func TestTakeoverRequiresMatchingOwnerAndToken(t *testing.T) {
	reg := NewRegistry()
	clk := newClock()
	vessel := uuid.New()

	if _, err := reg.Takeover(vessel, "A", 1, "B", time.Minute, clk.now()); !errors.Is(err, ErrNoLease) {
		t.Fatalf("takeover with no lease = %v, want ErrNoLease", err)
	}

	leaseA, err := reg.Assign(vessel, "A", time.Minute, clk.now())
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	cases := []struct {
		name   string
		owner  string
		token  uint64
		target string
		want   error
	}{
		{"mismatched previous owner", "intruder", leaseA.FencingToken, "B", ErrNotAuthorized},
		{"stale fencing token", "A", leaseA.FencingToken + 99, "B", ErrNotAuthorized},
		{"missing token", "A", 0, "B", ErrNotAuthorized},
		{"same owner", "A", leaseA.FencingToken, "A", ErrIdenticalOwners},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.Takeover(vessel, tc.owner, tc.token, tc.target, time.Minute, clk.now())
			if !errors.Is(err, tc.want) {
				t.Fatalf("takeover = %v, want %v", err, tc.want)
			}
		})
	}

	// Every rejected attempt must leave A in control with the original token.
	current, ok := reg.Get(vessel)
	if !ok {
		t.Fatal("lease vanished")
	}
	if current.Owner != "A" || current.FencingToken != leaseA.FencingToken {
		t.Fatalf("lease mutated by rejected takeover: %+v", current)
	}
}

func TestTakeoverTransfersAndInvalidatesOldToken(t *testing.T) {
	reg := NewRegistry()
	clk := newClock()
	vessel := uuid.New()

	leaseA, err := reg.Assign(vessel, "A", time.Minute, clk.now())
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	// B presents A's exact owner identity and current fencing token.
	leaseB, err := reg.Takeover(vessel, "A", leaseA.FencingToken, "B", time.Minute, clk.now())
	if err != nil {
		t.Fatalf("authorized takeover: %v", err)
	}
	if leaseB.Owner != "B" {
		t.Fatalf("new owner = %q, want B", leaseB.Owner)
	}
	if leaseB.FencingToken != leaseA.FencingToken+1 {
		t.Fatalf("new token = %d, want %d", leaseB.FencingToken, leaseA.FencingToken+1)
	}

	// The old fence token is dead immediately: A can neither renew nor release.
	if _, err := reg.Renew(vessel, "A", leaseA.FencingToken, time.Minute, clk.now()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("renew with old token = %v, want ErrNotAuthorized", err)
	}
	if reg.Release(vessel, "A", leaseA.FencingToken) {
		t.Fatal("release with stale token succeeded")
	}

	// B renews with its own token; the token value is preserved across renewal.
	clk.advance(10 * time.Second)
	renewed, err := reg.Renew(vessel, "B", leaseB.FencingToken, time.Minute, clk.now())
	if err != nil {
		t.Fatalf("B renew: %v", err)
	}
	if renewed.FencingToken != leaseB.FencingToken || renewed.Owner != "B" {
		t.Fatalf("renewal altered identity: %+v", renewed)
	}
}

func TestTakeoverAndRenewalRejectExpiredLease(t *testing.T) {
	reg := NewRegistry()
	clk := newClock()
	vessel := uuid.New()

	leaseA, err := reg.Assign(vessel, "A", time.Minute, clk.now())
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	clk.advance(61 * time.Second)

	if _, err := reg.Takeover(vessel, "A", leaseA.FencingToken, "B", time.Minute, clk.now()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("takeover expired = %v, want ErrLeaseExpired", err)
	}
	if _, err := reg.Renew(vessel, "A", leaseA.FencingToken, time.Minute, clk.now()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("renew expired = %v, want ErrLeaseExpired", err)
	}
}

func TestTakeoverCannotBePreemptedByConcurrentRenewal(t *testing.T) {
	reg := NewRegistry()
	clk := newClock()
	vessel := uuid.New()
	leaseA, err := reg.Assign(vessel, "A", time.Minute, clk.now())
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	const renewers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	var renewOK, renewStale int64
	var renewMu sync.Mutex
	for i := 0; i < renewers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := reg.Renew(vessel, "A", leaseA.FencingToken, time.Minute, clk.now())
			renewMu.Lock()
			if err == nil {
				renewOK++
			} else if errors.Is(err, ErrNotAuthorized) {
				renewStale++
			}
			renewMu.Unlock()
		}()
	}
	wg.Add(1)
	var takeoverErr error
	var leaseB model.Lease
	go func() {
		defer wg.Done()
		<-start
		leaseB, takeoverErr = reg.Takeover(vessel, "A", leaseA.FencingToken, "B", time.Minute, clk.now())
	}()
	close(start)
	wg.Wait()

	if takeoverErr != nil {
		t.Fatalf("takeover: %v", takeoverErr)
	}
	if renewOK+renewStale != renewers {
		t.Fatalf("unexpected renewal outcomes: ok=%d stale=%d", renewOK, renewStale)
	}

	final, ok := reg.Get(vessel)
	if !ok {
		t.Fatal("lease missing after concurrent operations")
	}
	if final.Owner != "B" || final.FencingToken != leaseB.FencingToken {
		t.Fatalf("final lease = %+v, want B/%d", final, leaseB.FencingToken)
	}
	// A's renewal either happened strictly before the takeover (lease still A
	// at that instant) or was rejected as stale afterwards — it can never
	// leave A's token valid once B owns the lease.
	if final.FencingToken == leaseA.FencingToken {
		t.Fatal("old fencing token still current after transfer")
	}
}

func newTestService(t *testing.T) (*Service, *journal.MemoryStore, *clock) {
	t.Helper()
	clk := newClock()
	store := journal.NewMemoryStore()
	recorder := journal.NewRecorder(store, clk.now)
	controller := admission.NewController(admission.NewQueue(), gate.NewState("chamber-1"), signal.NewStore(), clk.now)
	svc := NewService(NewRegistry(), controller, recorder, clk.now)
	return svc, store, clk
}

func TestServiceTakeoverJournalsNewToken(t *testing.T) {
	svc, store, clk := newTestService(t)
	ctx := context.Background()
	const chamber = "chamber-1"
	vessel := uuid.New()

	leaseA, err := svc.Assign(ctx, chamber, vessel, "A", time.Minute)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	before := len(mustEvents(t, ctx, store, chamber))
	leaseB, err := svc.Takeover(ctx, chamber, vessel, "A", leaseA.FencingToken, "B", time.Minute)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	events := mustEvents(t, ctx, store, chamber)
	if len(events) != before+1 {
		t.Fatalf("event count = %d, want %d", len(events), before+1)
	}
	event := events[len(events)-1]
	if event.Type != model.EventLeaseChanged {
		t.Fatalf("event type = %q, want lease.changed", event.Type)
	}
	if event.Generation != leaseB.FencingToken {
		t.Fatalf("journaled token = %d, want %d", event.Generation, leaseB.FencingToken)
	}
	var journaled model.Lease
	if err := event.Decode(&journaled); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if journaled.Owner != "B" || journaled.FencingToken != leaseB.FencingToken {
		t.Fatalf("journaled lease = %+v, want B/%d", journaled, leaseB.FencingToken)
	}

	// Token, log and admission authorization agree: old denied, new allowed.
	if svc.Authorized(vessel, "A", leaseA.FencingToken) {
		t.Fatal("admission still honors A's old fencing token")
	}
	if !svc.Authorized(vessel, "B", leaseB.FencingToken) {
		t.Fatal("admission denies B's new fencing token")
	}
	if svc.Authorized(vessel, "B", leaseA.FencingToken) {
		t.Fatal("admission accepted B with A's old token")
	}

	clk.advance(61 * time.Second)
	if svc.Authorized(vessel, "B", leaseB.FencingToken) {
		t.Fatal("admission honored an expired lease")
	}
}

func TestServiceRejectedTakeoverWritesNoEvent(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()
	const chamber = "chamber-1"
	vessel := uuid.New()

	if _, err := svc.Assign(ctx, chamber, vessel, "A", time.Minute); err != nil {
		t.Fatalf("assign: %v", err)
	}
	before := len(mustEvents(t, ctx, store, chamber))

	if _, err := svc.Takeover(ctx, chamber, vessel, "wrong-owner", 1, "B", time.Minute); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("takeover = %v, want ErrNotAuthorized", err)
	}
	if got := len(mustEvents(t, ctx, store, chamber)); got != before {
		t.Fatalf("rejected takeover wrote %d event(s)", got-before)
	}

	if _, err := svc.Takeover(ctx, "", vessel, "A", 1, "B", time.Minute); err == nil {
		t.Fatal("takeover without chamber accepted")
	}
}

func mustEvents(t *testing.T, ctx context.Context, store *journal.MemoryStore, chamber string) []model.Event {
	t.Helper()
	events, err := store.Events(ctx, chamber, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}
