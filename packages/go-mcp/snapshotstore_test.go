package gomcp

import (
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seqTokenSource returns a deterministic token source producing distinct
// TokenBytes-wide tokens so every mint yields a distinct, real-length handle.
func seqTokenSource() func() ([]byte, error) {
	n := 0
	return func() ([]byte, error) {
		n++
		b := make([]byte, TokenBytes)
		b[0] = byte(n)
		b[1] = byte(n >> 8)
		b[2] = byte(n >> 16)
		return b, nil
	}
}

// fakeClock is an injectable, advanceable clock.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestStore() (*SnapshotStore[*string], *fakeClock, *[]string) {
	clk := &fakeClock{t: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)}
	var logs []string
	store := NewSnapshotStore[*string](clk.now, seqTokenSource(), func(m string) { logs = append(logs, m) })
	return store, clk, &logs
}

func target(app string) TargetKey { return TargetKey{App: app} }

func payload(s string) *string { return &s }

func TestMintResolve(t *testing.T) {
	store, _, _ := newTestStore()
	rec, err := store.Mint(target("com.example"), payload("body"), SnapshotMeta{AppName: "Example", PID: 7})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(rec.Handle, HandlePrefix) || !ValidHandleFormat(rec.Handle) {
		t.Fatalf("handle format: %q", rec.Handle)
	}
	if rec.Generation != 1 {
		t.Fatalf("generation = %d, want 1", rec.Generation)
	}
	got, err := store.Resolve(rec.Handle)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Payload == nil || *got.Payload != "body" || got.Meta.PID != 7 {
		t.Fatalf("resolved record = %+v", got)
	}
	if c := store.Counters(); c.Minted != 1 || c.Resolved != 1 {
		t.Fatalf("counters = %+v", c)
	}
}

func TestHandleFormatBase64URL(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})
	enc := rec.Handle[len(HandlePrefix):]
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(b) != TokenBytes {
		t.Fatalf("token not unpadded base64url of %d bytes: err=%v len=%d", TokenBytes, err, len(b))
	}
	if strings.ContainsAny(enc, "=+/") {
		t.Fatalf("handle uses padded/standard base64: %q", enc)
	}
}

func TestTTLBoundaryExpiry(t *testing.T) {
	store, clk, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})

	// One nanosecond before absolute expiry: still live.
	clk.t = rec.ExpiresAt.Add(-time.Nanosecond)
	if _, err := store.Resolve(rec.Handle); err != nil {
		t.Fatalf("resolve just before expiry: %v", err)
	}

	// At the absolute expiry instant: expired.
	clk.t = rec.ExpiresAt
	_, err := store.Resolve(rec.Handle)
	var re *ResolveError
	if !errors.As(err, &re) || re.Code() != ErrSnapshotRefExpired {
		t.Fatalf("resolve at expiry = %v, want expired", err)
	}
	if store.Counters().Expired != 1 {
		t.Fatalf("expired counter = %d", store.Counters().Expired)
	}
}

func TestCapacityEvictionOrder(t *testing.T) {
	store, _, _ := newTestStore()
	handles := make([]string, 0, MaxLiveTargets+1)
	for i := 0; i < MaxLiveTargets+1; i++ {
		rec, err := store.Mint(target(string(rune('a'+i))), payload("x"), SnapshotMeta{})
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		handles = append(handles, rec.Handle)
	}
	// The least-recently-created target is evicted; everything after it stays.
	_, err := store.Resolve(handles[0])
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reasonEvicted || re.Code() != ErrSnapshotRefExpired {
		t.Fatalf("oldest handle = %v, want evicted", err)
	}
	for i := 1; i < len(handles); i++ {
		if _, err := store.Resolve(handles[i]); err != nil {
			t.Fatalf("handle %d unexpectedly gone: %v", i, err)
		}
	}
	if len(store.live) != MaxLiveTargets {
		t.Fatalf("live count = %d, want %d", len(store.live), MaxLiveTargets)
	}
	if store.Counters().Evicted != 1 {
		t.Fatalf("evicted counter = %d", store.Counters().Evicted)
	}
}

func TestTombstoneStaleVsUnknown(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})
	if err := store.Supersede(rec.Handle); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	// A superseded handle is stale (tombstoned), not unknown.
	_, err := store.Resolve(rec.Handle)
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reasonSuperseded || re.Code() != ErrSnapshotRefStale {
		t.Fatalf("superseded resolve = %v, want stale", err)
	}
	// A handle the store never minted is unknown.
	unmintedToken := make([]byte, TokenBytes)
	unmintedToken[0] = 0xff
	unminted := HandlePrefix + base64.RawURLEncoding.EncodeToString(unmintedToken)
	_, err = store.Resolve(unminted)
	if !errors.As(err, &re) || re.Reason != reasonUnknown || re.Code() != ErrSnapshotRefUnknown {
		t.Fatalf("unminted resolve = %v, want unknown", err)
	}
}

func TestTombstoneCapacityFIFO(t *testing.T) {
	store, _, _ := newTestStore()
	// Re-minting the same target supersedes the prior live handle, producing one
	// tombstone per re-mint while keeping exactly one live target (no eviction).
	handles := make([]string, 0, MaxTombstones+2)
	for i := 0; i < MaxTombstones+2; i++ {
		rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})
		handles = append(handles, rec.Handle)
	}
	// MaxTombstones+1 supersede tombstones were produced; the oldest overflowed.
	if len(store.tombstones) != MaxTombstones {
		t.Fatalf("tombstones = %d, want %d", len(store.tombstones), MaxTombstones)
	}
	_, err := store.Resolve(handles[0])
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reasonUnknown {
		t.Fatalf("overflowed tombstone = %v, want unknown", err)
	}
	_, err = store.Resolve(handles[1])
	if !errors.As(err, &re) || re.Reason != reasonSuperseded {
		t.Fatalf("retained tombstone = %v, want superseded", err)
	}
	// The final handle is still live.
	if _, err := store.Resolve(handles[len(handles)-1]); err != nil {
		t.Fatalf("final handle should be live: %v", err)
	}
}

func TestRedactionNoFullHandleInLogsOrErrors(t *testing.T) {
	// A distinct store per reason so each error path is exercised in isolation.
	assertRedacted := func(t *testing.T, err error, handle string) {
		t.Helper()
		if err == nil {
			t.Fatal("expected a resolve error")
		}
		if strings.Contains(err.Error(), handle) {
			t.Fatalf("error string leaked full handle: %q", err.Error())
		}
		if !strings.Contains(err.Error(), handle[len(handle)-6:]) {
			t.Fatalf("error missing redacted suffix: %q", err.Error())
		}
	}

	// stale (superseded)
	store, _, logs := newTestStore()
	stale, _ := store.Mint(target("a"), payload("secret"), SnapshotMeta{})
	_ = store.Supersede(stale.Handle)
	_, staleErr := store.Resolve(stale.Handle)
	assertRedacted(t, staleErr, stale.Handle)

	// expired
	expStore, expClk, _ := newTestStore()
	expired, _ := expStore.Mint(target("a"), payload("secret"), SnapshotMeta{})
	expClk.t = expired.ExpiresAt
	_, expErr := expStore.Resolve(expired.Handle)
	assertRedacted(t, expErr, expired.Handle)

	// evicted (oldest live over capacity)
	evStore, _, _ := newTestStore()
	var evictedHandle string
	for i := 0; i < MaxLiveTargets+1; i++ {
		rec, _ := evStore.Mint(target(string(rune('a'+i))), payload("secret"), SnapshotMeta{})
		if i == 0 {
			evictedHandle = rec.Handle
		}
	}
	_, evErr := evStore.Resolve(evictedHandle)
	assertRedacted(t, evErr, evictedHandle)

	// unknown (valid format, never minted)
	unknownToken := make([]byte, TokenBytes)
	unknownToken[0] = 0xab
	unknown := HandlePrefix + base64.RawURLEncoding.EncodeToString(unknownToken)
	_, unkErr := store.Resolve(unknown)
	assertRedacted(t, unkErr, unknown)

	// malformed (wrong shape, still long enough to leak a middle if unredacted)
	malformed := "leaked_prefix_" + strings.Repeat("Z", 40) + "abcdef"
	_, malErr := store.Resolve(malformed)
	assertRedacted(t, malErr, malformed)
	var re *ResolveError
	if !errors.As(malErr, &re) || re.Code() != ErrSnapshotRefMalformed {
		t.Fatalf("malformed resolve = %v, want malformed", malErr)
	}

	// No lifecycle log line ever carries a full handle.
	if len(*logs) == 0 {
		t.Fatal("expected redacted lifecycle logs")
	}
	for _, line := range *logs {
		if strings.Contains(line, stale.Handle) {
			t.Fatalf("log line leaked full handle: %q", line)
		}
	}
}

func TestResolveMalformedHandle(t *testing.T) {
	store, _, _ := newTestStore()
	for _, bad := range []string{
		"",
		"not-a-handle",
		"ocu_snapshot_v1_",
		HandlePrefix + "short",
		HandlePrefix + "not+base64url==",
	} {
		_, err := store.Resolve(bad)
		var re *ResolveError
		if !errors.As(err, &re) || re.Reason != reasonMalformed || re.Code() != ErrSnapshotRefMalformed {
			t.Errorf("Resolve(%q) = %v, want malformed", bad, err)
		}
	}
	// A valid-format but never-minted handle is unknown, not malformed.
	tok := make([]byte, TokenBytes)
	tok[0] = 0x01
	unminted := HandlePrefix + base64.RawURLEncoding.EncodeToString(tok)
	_, err := store.Resolve(unminted)
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reasonUnknown {
		t.Fatalf("valid-format unminted = %v, want unknown", err)
	}
}

// TestLimitConstants pins the literal limit values so a change is a deliberate,
// reviewed edit rather than an accident.
func TestLimitConstants(t *testing.T) {
	if DefaultTTL != 120*time.Second {
		t.Errorf("DefaultTTL = %v, want 120s", DefaultTTL)
	}
	if MaxLiveTargets != 16 {
		t.Errorf("MaxLiveTargets = %d, want 16", MaxLiveTargets)
	}
	if MaxTombstones != 64 {
		t.Errorf("MaxTombstones = %d, want 64", MaxTombstones)
	}
	if TokenBytes != 24 {
		t.Errorf("TokenBytes = %d, want 24", TokenBytes)
	}
}

func TestFreshStoreUnknownRestartEquivalence(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})

	// A new store instance models a process restart: no tombstones, so the prior
	// handle is a recoverable unknown, never a false live hit.
	fresh := NewSnapshotStore[*string](store.now, seqTokenSource(), nil)
	_, err := fresh.Resolve(rec.Handle)
	var re *ResolveError
	if !errors.As(err, &re) || re.Code() != ErrSnapshotRefUnknown {
		t.Fatalf("fresh-store resolve = %v, want unknown", err)
	}
}

func TestOneLiveGenerationSupersedeOnReMint(t *testing.T) {
	store, _, _ := newTestStore()
	first, _ := store.Mint(target("a"), payload("one"), SnapshotMeta{})
	second, _ := store.Mint(target("a"), payload("two"), SnapshotMeta{})

	if second.Generation != first.Generation+1 {
		t.Fatalf("generation did not advance: %d -> %d", first.Generation, second.Generation)
	}
	if len(store.live) != 1 {
		t.Fatalf("re-mint left %d live records, want 1", len(store.live))
	}
	// The prior generation is superseded (stale), the new one live.
	_, err := store.Resolve(first.Handle)
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reasonSuperseded {
		t.Fatalf("first handle = %v, want superseded", err)
	}
	if _, err := store.Resolve(second.Handle); err != nil {
		t.Fatalf("second handle should be live: %v", err)
	}
}

func TestPayloadReleaseOnSupersede(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("body"), SnapshotMeta{})

	// White-box: hold the internal record pointer, then supersede and assert its
	// payload was nil'd so the garbage collector can reclaim it.
	internal := store.live[rec.Handle]
	if internal.Payload == nil {
		t.Fatal("live record should hold a payload")
	}
	_ = store.Supersede(rec.Handle)
	if internal.Payload != nil {
		t.Fatalf("payload not released on supersede: %v", *internal.Payload)
	}
	if _, ok := store.tombstones[rec.Handle]; !ok {
		t.Fatal("expected a tombstone after supersede")
	}
}

func TestPayloadReleaseOnExpiry(t *testing.T) {
	store, clk, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("body"), SnapshotMeta{})
	internal := store.live[rec.Handle]
	clk.t = rec.ExpiresAt
	_, _ = store.Resolve(rec.Handle)
	if internal.Payload != nil {
		t.Fatalf("payload not released on expiry: %v", *internal.Payload)
	}
}

// TestResolveComparesFullHandle proves resolve compares the whole handle (the
// constant-time path in lookupLiveLocked), not a prefix: a handle differing only
// in its final byte does not resolve to a stored record.
func TestResolveComparesFullHandle(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})

	last := rec.Handle[len(rec.Handle)-1]
	flip := byte('A')
	if last == 'A' {
		flip = 'B'
	}
	near := rec.Handle[:len(rec.Handle)-1] + string(flip)
	if near == rec.Handle {
		t.Fatal("failed to construct a near-miss handle")
	}
	_, err := store.Resolve(near)
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reasonUnknown {
		t.Fatalf("near-miss handle = %v, want unknown", err)
	}
	// The genuine handle still resolves.
	if _, err := store.Resolve(rec.Handle); err != nil {
		t.Fatalf("genuine handle failed: %v", err)
	}
}

// TestConcurrentMintResolveSupersede drives many goroutines against one store
// (run under -race). It asserts no panic, the live and tombstone caps hold, and
// the lifecycle counters stay consistent: every minted record is either still
// live or accounted for by exactly one of stale/evicted/expired.
func TestConcurrentMintResolveSupersede(t *testing.T) {
	store := NewSnapshotStore[*string](nil, nil, nil)
	const goroutines = 16
	const iters = 200

	targets := make([]string, 24)
	for i := range targets {
		targets[i] = "app-" + string(rune('a'+i))
	}

	var mints int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				tk := TargetKey{App: targets[(g+i)%len(targets)]}
				body := "b"
				rec, err := store.Mint(tk, &body, SnapshotMeta{AppName: tk.App})
				if err != nil {
					t.Errorf("mint: %v", err)
					return
				}
				atomic.AddInt64(&mints, 1)
				// The handle may already be superseded or evicted by another
				// goroutine; either outcome is valid, only a panic is not.
				_, _ = store.Resolve(rec.Handle)
				if i%5 == 0 {
					_ = store.Supersede(rec.Handle)
				}
			}
		}(g)
	}
	wg.Wait()

	c := store.Counters()
	if int64(c.Minted) != mints {
		t.Fatalf("Minted = %d, want %d", c.Minted, mints)
	}
	if len(store.live) > MaxLiveTargets {
		t.Fatalf("live count %d exceeds cap %d", len(store.live), MaxLiveTargets)
	}
	if len(store.tombstones) > MaxTombstones {
		t.Fatalf("tombstone count %d exceeds cap %d", len(store.tombstones), MaxTombstones)
	}
	if c.Minted != len(store.live)+c.Stale+c.Evicted+c.Expired {
		t.Fatalf("counter sum mismatch: minted=%d live=%d stale=%d evicted=%d expired=%d",
			c.Minted, len(store.live), c.Stale, c.Evicted, c.Expired)
	}
}

func TestValidHandleFormat(t *testing.T) {
	store, _, _ := newTestStore()
	rec, _ := store.Mint(target("a"), payload("x"), SnapshotMeta{})
	if !ValidHandleFormat(rec.Handle) {
		t.Fatalf("minted handle rejected: %q", rec.Handle)
	}
	for _, bad := range []string{
		"",
		"ocu_snapshot_v1_",
		"nope_" + rec.Handle,
		HandlePrefix + "not+base64url==",
		HandlePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes-1)),
	} {
		if ValidHandleFormat(bad) {
			t.Errorf("accepted malformed handle: %q", bad)
		}
	}
}
