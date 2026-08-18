package gomcp

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestBeginActionMovesLiveToInFlight verifies the CAS: a live handle begins an
// action once, and a second begin of the same handle loses with in_use while the
// first is still in flight.
func TestBeginActionMovesLiveToInFlight(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("body"), SnapshotMeta{AppName: "A"})

	got, err := store.BeginAction(rec.Handle)
	if err != nil {
		t.Fatalf("first BeginAction: %v", err)
	}
	if got.Lifecycle != LifecycleInFlight {
		t.Fatalf("record not in_flight: %v", got.Lifecycle)
	}
	if got.Payload == nil || *got.Payload != "body" {
		t.Fatalf("in_flight record lost payload: %+v", got.Payload)
	}

	_, err = store.BeginAction(rec.Handle)
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reasonInUse || re.Code() != ErrSnapshotRefInUse {
		t.Fatalf("second BeginAction = %v, want in_use", err)
	}
	if CanonicalRetry(re.Code()) != RetrySameHandle {
		t.Fatalf("in_use retry = %q, want same_handle", CanonicalRetry(re.Code()))
	}
	if store.Counters().Concurrent != 1 {
		t.Fatalf("concurrent counter = %d, want 1", store.Counters().Concurrent)
	}
}

func TestBeginActionRejectsBadHandles(t *testing.T) {
	store, clk, _ := newTestStore()

	if _, err := store.BeginAction("not-a-handle"); err == nil {
		t.Fatal("malformed handle accepted")
	} else {
		var re *ResolveError
		if !errors.As(err, &re) || re.Code() != ErrSnapshotRefMalformed {
			t.Fatalf("malformed = %v", err)
		}
	}

	// A superseded handle begins as stale, an unminted one as unknown.
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})
	_ = store.Supersede(rec.Handle)
	if _, err := store.BeginAction(rec.Handle); err == nil {
		t.Fatal("stale handle accepted")
	} else {
		var re *ResolveError
		if !errors.As(err, &re) || re.Code() != ErrSnapshotRefStale {
			t.Fatalf("stale = %v", err)
		}
	}

	live, _ := store.Mint(target("b"), payload("x"), SnapshotMeta{})
	clk.t = live.ExpiresAt
	if _, err := store.BeginAction(live.Handle); err == nil {
		t.Fatal("expired handle accepted")
	} else {
		var re *ResolveError
		if !errors.As(err, &re) || re.Code() != ErrSnapshotRefExpired {
			t.Fatalf("expired = %v", err)
		}
	}
}

// TestFinishSuccessMintsSuccessor verifies a completed action supersedes the old
// handle (which then resolves stale) and mints a successor at generation n+1 for
// the same target that resolves to the fresh payload.
func TestFinishSuccessMintsSuccessor(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("before"), SnapshotMeta{AppName: "A"})
	if _, err := store.BeginAction(rec.Handle); err != nil {
		t.Fatalf("begin: %v", err)
	}

	succ, err := store.FinishSuccess(rec.Handle, payload("after"), SnapshotMeta{AppName: "A"})
	if err != nil {
		t.Fatalf("FinishSuccess: %v", err)
	}
	if succ.Generation != rec.Generation+1 {
		t.Fatalf("successor generation = %d, want %d", succ.Generation, rec.Generation+1)
	}
	if succ.Handle == rec.Handle {
		t.Fatal("successor reused the old handle")
	}

	// Old handle is stale; successor resolves live to the fresh payload.
	_, err = store.Resolve(rec.Handle)
	var re *ResolveError
	if !errors.As(err, &re) || re.Code() != ErrSnapshotRefStale {
		t.Fatalf("old handle = %v, want stale", err)
	}
	got, err := store.Resolve(succ.Handle)
	if err != nil || got.Payload == nil || *got.Payload != "after" {
		t.Fatalf("successor resolve = %+v err=%v", got.Payload, err)
	}
	if len(store.live) != 1 {
		t.Fatalf("live count = %d, want 1", len(store.live))
	}

	// A second action can run against the successor.
	if _, err := store.BeginAction(succ.Handle); err != nil {
		t.Fatalf("begin on successor: %v", err)
	}
}

// TestAbortRestoreReturnsToLive verifies a pre-dispatch abort restores the handle
// to live so a retry with the same reference succeeds, and that the mismatch
// reason bumps the Mismatched counter.
func TestAbortRestoreReturnsToLive(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})

	if _, err := store.BeginAction(rec.Handle); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.AbortRestore(rec.Handle, AbortValidation); err != nil {
		t.Fatalf("AbortRestore: %v", err)
	}
	if store.Counters().Mismatched != 0 {
		t.Fatalf("validation abort bumped Mismatched: %d", store.Counters().Mismatched)
	}
	// Restored: it can begin again.
	if _, err := store.BeginAction(rec.Handle); err != nil {
		t.Fatalf("re-begin after restore: %v", err)
	}
	if err := store.AbortRestore(rec.Handle, AbortMismatched); err != nil {
		t.Fatalf("AbortRestore mismatch: %v", err)
	}
	if store.Counters().Mismatched != 1 {
		t.Fatalf("mismatch abort Mismatched = %d, want 1", store.Counters().Mismatched)
	}
	if _, err := store.Resolve(rec.Handle); err != nil {
		t.Fatalf("handle should be live after restore: %v", err)
	}
}

// TestAbortSupersedeInvalidates verifies the uncertain and refresh-failed aborts
// supersede the handle (stale afterward) and record the right counters.
func TestAbortSupersedeInvalidates(t *testing.T) {
	for _, tc := range []struct {
		reason   string
		counter  func(SnapshotCounters) int
		wantName string
	}{
		{AbortUncertain, func(c SnapshotCounters) int { return c.Uncertain }, "uncertain"},
		{AbortRefreshFailed, func(c SnapshotCounters) int { return c.RefreshFailed }, "refresh_failed"},
	} {
		t.Run(tc.wantName, func(t *testing.T) {
			store, _, _ := newTestStore()
			rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})
			if _, err := store.BeginAction(rec.Handle); err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := store.AbortSupersede(rec.Handle, tc.reason); err != nil {
				t.Fatalf("AbortSupersede: %v", err)
			}
			_, err := store.Resolve(rec.Handle)
			var re *ResolveError
			if !errors.As(err, &re) || re.Code() != ErrSnapshotRefStale {
				t.Fatalf("handle after supersede = %v, want stale", err)
			}
			if tc.counter(store.Counters()) != 1 {
				t.Fatalf("%s counter = %d, want 1", tc.wantName, tc.counter(store.Counters()))
			}
			// A failed action is not counted as Stale (only FinishSuccess is).
			if store.Counters().Stale != 0 {
				t.Fatalf("%s bumped Stale to %d, want 0", tc.wantName, store.Counters().Stale)
			}
		})
	}
}

// TestConcurrentBeginActionSingleWinner drives many goroutines at one handle with
// a blocking gate and asserts exactly one wins the CAS (the rest get in_use),
// proving one handle never double-dispatches. Run under -race.
func TestConcurrentBeginActionSingleWinner(t *testing.T) {
	store := NewSnapshotStore[*string](nil, nil, nil)
	body := "x"
	rec, _ := store.Mint(TargetKey{App: "a"}, &body, SnapshotMeta{AppName: "a"})

	const goroutines = 32
	var winners int64
	var inUse int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := store.BeginAction(rec.Handle); err == nil {
				atomic.AddInt64(&winners, 1)
			} else {
				var re *ResolveError
				if errors.As(err, &re) && re.Code() == ErrSnapshotRefInUse {
					atomic.AddInt64(&inUse, 1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if inUse != goroutines-1 {
		t.Fatalf("in_use losers = %d, want %d", inUse, goroutines-1)
	}
	if store.Counters().Concurrent != goroutines-1 {
		t.Fatalf("concurrent counter = %d, want %d", store.Counters().Concurrent, goroutines-1)
	}
}
