package gomcp

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Bounded snapshot handle store. get_app_state mints an opaque handle for a
// captured window; the record owns the screenshot and element references so the
// handle itself carries no user data. Handles are unguessable capability
// references, not authentication. The store is generic over the payload type so
// each platform keeps its own snapshot value out of this package; releasing a
// record nils the payload so the garbage collector reclaims screenshot bytes and
// native element references on expiry or supersede. Actions do not consume
// handles yet; that lands with M4, which reuses this file's per-target
// generation tracking and internal lock.

// HandlePrefix is the fixed, versioned prefix every snapshot handle carries. A
// handle is HandlePrefix + base64url(TokenBytes random bytes) with no padding.
const HandlePrefix = "ocu_snapshot_v1_"

// TokenBytes is the random-byte width behind each handle.
const TokenBytes = 24

// Store limits. These are constants with unit tests, not user configuration in
// the first release: a 120-second absolute (non-sliding) TTL, one live
// generation per target, at most 16 live targets, and at most 64 lightweight
// tombstones used to distinguish stale from unknown handles.
const (
	DefaultTTL     = 120 * time.Second
	MaxLiveTargets = 16
	MaxTombstones  = 64
)

// Tombstone reasons. A tombstone records only a redacted suffix, its reason, and
// a timestamp; it never retains the payload.
const (
	reasonExpired    = "expired"
	reasonSuperseded = "superseded"
	reasonEvicted    = "evicted"
	reasonUnknown    = "unknown"
	reasonMalformed  = "malformed"
	// reasonInUse is not a tombstone reason: it is returned by BeginAction when a
	// handle is already in_flight, so a second concurrent use loses the CAS. It is
	// never stored in the tombstone table.
	reasonInUse = "in_use"
)

// Abort reasons attribute a failed action transaction to a lifecycle counter.
// The apps pass one of these to AbortRestore/AbortSupersede so the store records
// the failure class at transition time. AbortMismatched restores a handle to live
// (the requested app or the runtime-resolved element did not match the stored
// target; zero native action occurred). AbortValidation restores a handle to live
// for a plain pre-dispatch input-validation failure and records no failure-class
// counter. AbortUncertain and AbortRefreshFailed supersede the handle after a
// dispatch whose outcome is unknown or whose post-action recapture failed.
const (
	AbortMismatched    = "mismatched"
	AbortValidation    = "validation"
	AbortUncertain     = "uncertain"
	AbortRefreshFailed = "refresh_failed"
)

// SnapshotLifecycle is the state of a stored record. M3 mints records live; the
// other states are set by tombstoning or by the M4 action transaction.
type SnapshotLifecycle string

const (
	LifecycleLive       SnapshotLifecycle = "live"
	LifecycleInFlight   SnapshotLifecycle = "in_flight"
	LifecycleSuperseded SnapshotLifecycle = "superseded"
	LifecycleExpired    SnapshotLifecycle = "expired"
)

// TargetKey identifies the window a handle is bound to. Generation is monotonic
// per target: re-minting the same target supersedes the previous live handle and
// increments the generation. App is the normalized app identity (bundle or
// executable id where available, otherwise the app name); Window is the stable
// window id where the platform provides one, otherwise empty.
type TargetKey struct {
	App    string
	Window string
}

// SnapshotMeta is the normalized metadata stored with a handle. It is small and
// safe to keep; the screenshot pixels and element records live in the payload.
// BundleIdentifier and WindowID are empty when the platform does not expose
// them; ScreenshotPixels is nil when the pixel dimensions could not be derived.
type SnapshotMeta struct {
	AppName          string
	BundleIdentifier string
	PID              int
	WindowID         string
	Bounds           Rect
	ScreenshotPixels *Size
	Mode             string
	CaptureOptions   map[string]any
}

// SnapshotRecord is a live handle's stored state. Payload holds the platform
// snapshot value (screenshot and element references) and is nil once the record
// is released.
type SnapshotRecord[P any] struct {
	Handle     string
	Target     TargetKey
	Generation int
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Meta       SnapshotMeta
	Payload    P
	Lifecycle  SnapshotLifecycle

	// createSeq breaks CreatedAt ties so least-recently-created eviction is
	// deterministic under a fake clock that returns equal timestamps.
	createSeq int
}

// tombstone is the lightweight residue of a retired handle: reason plus a
// timestamp, no payload.
type tombstone struct {
	reason    string
	retiredAt time.Time
}

// SnapshotCounters are the redacted lifecycle counters emitted to stderr. The
// action-transaction counters (Mismatched, Concurrent, Uncertain, RefreshFailed)
// are recorded at transition time by the M4 primitives.
type SnapshotCounters struct {
	Minted   int
	Resolved int
	Expired  int
	Stale    int
	Evicted  int
	// Mismatched counts action transactions aborted because the requested app or
	// the runtime-resolved element did not match the stored target.
	Mismatched int
	// Concurrent counts BeginAction calls that lost the live->in_flight CAS
	// because the handle was already in_flight (a second concurrent use).
	Concurrent int
	// Uncertain counts action transactions superseded because a dispatch began
	// but its outcome could not be determined.
	Uncertain int
	// RefreshFailed counts action transactions superseded because the action
	// dispatched but the post-action state could not be recaptured.
	RefreshFailed int
}

// SnapshotStore is a bounded, mutex-guarded handle store owned by the device
// runtime (the MCP process on Linux and Windows). All state transitions run
// under mu.
type SnapshotStore[P any] struct {
	mu          sync.Mutex
	now         func() time.Time
	tokenSource func() ([]byte, error)
	logger      func(string)
	ttl         time.Duration

	live       map[string]*SnapshotRecord[P]
	tombstones map[string]tombstone
	tombOrder  []string
	targetLive map[TargetKey]string
	gen        map[TargetKey]int
	counters   SnapshotCounters
	seq        int
}

// NewSnapshotStore builds a store. A nil now defaults to time.Now; a nil
// tokenSource defaults to crypto/rand; a nil logger disables logging. The clock
// and token source are injectable so tests are deterministic.
func NewSnapshotStore[P any](now func() time.Time, tokenSource func() ([]byte, error), logger func(string)) *SnapshotStore[P] {
	if now == nil {
		now = time.Now
	}
	if tokenSource == nil {
		tokenSource = cryptoTokenSource
	}
	return &SnapshotStore[P]{
		now:         now,
		tokenSource: tokenSource,
		logger:      logger,
		ttl:         DefaultTTL,
		live:        map[string]*SnapshotRecord[P]{},
		tombstones:  map[string]tombstone{},
		targetLive:  map[TargetKey]string{},
		gen:         map[TargetKey]int{},
	}
}

// cryptoTokenSource is the production token source: TokenBytes of CSPRNG bytes.
func cryptoTokenSource() ([]byte, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// Mint captures a new live handle for target, enforcing one live generation per
// target and the capacity limits. Expired records are retired first, then the
// prior live handle for this target is superseded, then least-recently-created
// live records are evicted until there is room.
func (s *SnapshotStore[P]) Mint(target TargetKey, payload P, meta SnapshotMeta) (SnapshotRecord[P], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	s.expireLocked(now)
	rec, err := s.mintLocked(target, payload, meta, now)
	if err != nil {
		return SnapshotRecord[P]{}, err
	}
	return *rec, nil
}

// mintLocked supersedes the prior live handle for target, evicts down to the
// capacity limit, and mints a fresh live record with the next generation. The
// caller holds mu and has already retired expired records. It is shared by Mint
// and by FinishSuccess (successor minting), so a successful action advances the
// same per-target generation an explicit re-capture would.
func (s *SnapshotStore[P]) mintLocked(target TargetKey, payload P, meta SnapshotMeta, now time.Time) (*SnapshotRecord[P], error) {
	if h, ok := s.targetLive[target]; ok {
		s.tombstoneLocked(h, reasonSuperseded, now)
		s.counters.Stale++
	}

	for len(s.live) >= MaxLiveTargets {
		if !s.evictOldestLocked(now) {
			// Every remaining live record is in_flight and must not be evicted; mint
			// above the soft cap rather than break an in-flight transaction.
			break
		}
	}

	token, err := s.tokenSource()
	if err != nil {
		return nil, err
	}
	if len(token) != TokenBytes {
		return nil, fmt.Errorf("snapshot token source returned %d bytes, want %d", len(token), TokenBytes)
	}
	handle := HandlePrefix + base64.RawURLEncoding.EncodeToString(token)

	s.gen[target]++
	s.seq++
	rec := &SnapshotRecord[P]{
		Handle:     handle,
		Target:     target,
		Generation: s.gen[target],
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.ttl),
		Meta:       meta,
		Payload:    payload,
		Lifecycle:  LifecycleLive,
		createSeq:  s.seq,
	}
	s.live[handle] = rec
	s.targetLive[target] = handle
	s.counters.Minted++
	s.logf("mint", handle)
	return rec, nil
}

// BeginAction opens the action transaction for a handle: it validates the format,
// resolves the handle, and moves it from live to in_flight under the store lock.
// It returns a *ResolveError for a malformed, unknown, expired, or stale handle,
// and a *ResolveError with reason in_use (code snapshot_ref_in_use, retry
// same_handle) when the handle is already in_flight. The compare-and-swap is the
// single-writer serialization point: of two concurrent uses of one handle, only
// the first receives the record and dispatches; the loser gets in_use.
func (s *SnapshotStore[P]) BeginAction(handle string) (SnapshotRecord[P], error) {
	if !ValidHandleFormat(handle) {
		return SnapshotRecord[P]{}, &ResolveError{Reason: reasonMalformed, Suffix: RedactHandle(handle)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	s.expireLocked(now)

	if rec := s.lookupLiveLocked(handle); rec != nil {
		if rec.Lifecycle == LifecycleInFlight {
			s.counters.Concurrent++
			s.logf(reasonInUse, handle)
			return SnapshotRecord[P]{}, &ResolveError{Reason: reasonInUse, Suffix: RedactHandle(handle)}
		}
		rec.Lifecycle = LifecycleInFlight
		s.counters.Resolved++
		s.logf("begin", handle)
		return *rec, nil
	}
	if reason, ok := s.lookupTombstoneLocked(handle); ok {
		return SnapshotRecord[P]{}, &ResolveError{Reason: reason, Suffix: RedactHandle(handle)}
	}
	return SnapshotRecord[P]{}, &ResolveError{Reason: reasonUnknown, Suffix: RedactHandle(handle)}
}

// FinishSuccess closes a successful action transaction: it supersedes the
// in_flight handle and mints its successor for the same target with the next
// generation, carrying the fresh post-action payload and metadata. The old handle
// then resolves as stale. It returns a *ResolveError when the handle is not
// in_flight (a caller invariant violation) or when minting the successor fails.
func (s *SnapshotStore[P]) FinishSuccess(handle string, payload P, meta SnapshotMeta) (SnapshotRecord[P], error) {
	if !ValidHandleFormat(handle) {
		return SnapshotRecord[P]{}, &ResolveError{Reason: reasonMalformed, Suffix: RedactHandle(handle)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	rec := s.inFlightLocked(handle)
	if rec == nil {
		return SnapshotRecord[P]{}, s.notInFlightErrorLocked(handle)
	}
	target := rec.Target
	// Retire the in_flight handle first so mintLocked does not try to supersede it
	// a second time through targetLive.
	s.tombstoneLocked(handle, reasonSuperseded, now)
	s.counters.Stale++
	succ, err := s.mintLocked(target, payload, meta, now)
	if err != nil {
		return SnapshotRecord[P]{}, err
	}
	return *succ, nil
}

// AbortRestore ends a failed pre-dispatch transaction by returning the in_flight
// handle to live so a corrected retry with the same reference is possible. The
// reason attributes the failure: AbortMismatched records a Mismatched counter (an
// app-identity or runtime element mismatch, both zero-dispatch); any other reason
// records no failure-class counter.
func (s *SnapshotStore[P]) AbortRestore(handle, reason string) error {
	if !ValidHandleFormat(handle) {
		return &ResolveError{Reason: reasonMalformed, Suffix: RedactHandle(handle)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.inFlightLocked(handle)
	if rec == nil {
		return s.notInFlightErrorLocked(handle)
	}
	rec.Lifecycle = LifecycleLive
	if reason == AbortMismatched {
		s.counters.Mismatched++
	}
	s.logf("abort_restore", handle)
	return nil
}

// AbortSupersede ends a transaction whose dispatch began but whose outcome is
// unknown (AbortUncertain) or whose post-action recapture failed
// (AbortRefreshFailed): it supersedes the in_flight handle so it can never be
// reused, and records the matching counter. The old handle then resolves as
// stale.
func (s *SnapshotStore[P]) AbortSupersede(handle, reason string) error {
	if !ValidHandleFormat(handle) {
		return &ResolveError{Reason: reasonMalformed, Suffix: RedactHandle(handle)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	rec := s.inFlightLocked(handle)
	if rec == nil {
		return s.notInFlightErrorLocked(handle)
	}
	// The handle becomes stale-RESOLVABLE (a superseded tombstone) but is not
	// counted as Stale: this is a failed action, attributed only to its dedicated
	// Uncertain/RefreshFailed counter. A genuine supersede-by-successor
	// (FinishSuccess) is what the Stale counter tracks.
	s.tombstoneLocked(handle, reasonSuperseded, now)
	switch reason {
	case AbortUncertain:
		s.counters.Uncertain++
	case AbortRefreshFailed:
		s.counters.RefreshFailed++
	}
	s.logf("abort_supersede", handle)
	return nil
}

// inFlightLocked returns the in_flight record for handle, or nil when the handle
// is absent or in any other lifecycle state.
func (s *SnapshotStore[P]) inFlightLocked(handle string) *SnapshotRecord[P] {
	rec := s.lookupLiveLocked(handle)
	if rec == nil || rec.Lifecycle != LifecycleInFlight {
		return nil
	}
	return rec
}

// notInFlightErrorLocked classifies a handle the transaction expected to find
// in_flight: a tombstoned handle keeps its tombstone reason, anything else is
// unknown. This is a caller-invariant guard; the normal transaction paths always
// hold an in_flight handle.
func (s *SnapshotStore[P]) notInFlightErrorLocked(handle string) error {
	if reason, ok := s.lookupTombstoneLocked(handle); ok {
		return &ResolveError{Reason: reason, Suffix: RedactHandle(handle)}
	}
	return &ResolveError{Reason: reasonUnknown, Suffix: RedactHandle(handle)}
}

// Resolve returns the live record for handle or a *ResolveError. A handle that
// does not match the pinned format is rejected as malformed before any table
// lookup; otherwise the tombstone table distinguishes unknown, expired, and
// superseded/evicted handles. A fresh store (process restart) has no tombstones,
// so a previously valid handle resolves to unknown. The handle comparison is
// constant-time.
func (s *SnapshotStore[P]) Resolve(handle string) (SnapshotRecord[P], error) {
	if !ValidHandleFormat(handle) {
		return SnapshotRecord[P]{}, &ResolveError{Reason: reasonMalformed, Suffix: RedactHandle(handle)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	s.expireLocked(now)

	if rec := s.lookupLiveLocked(handle); rec != nil {
		s.counters.Resolved++
		s.logf("resolve", handle)
		return *rec, nil
	}
	if reason, ok := s.lookupTombstoneLocked(handle); ok {
		return SnapshotRecord[P]{}, &ResolveError{Reason: reason, Suffix: RedactHandle(handle)}
	}
	return SnapshotRecord[P]{}, &ResolveError{Reason: reasonUnknown, Suffix: RedactHandle(handle)}
}

// Supersede retires a live handle, releasing its payload and recording a
// superseded tombstone. It reports a *ResolveError when the handle is not live.
func (s *SnapshotStore[P]) Supersede(handle string) error {
	if !ValidHandleFormat(handle) {
		return &ResolveError{Reason: reasonMalformed, Suffix: RedactHandle(handle)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	s.expireLocked(now)
	if rec := s.lookupLiveLocked(handle); rec != nil {
		s.tombstoneLocked(rec.Handle, reasonSuperseded, now)
		s.counters.Stale++
		return nil
	}
	if reason, ok := s.lookupTombstoneLocked(handle); ok {
		return &ResolveError{Reason: reason, Suffix: RedactHandle(handle)}
	}
	return &ResolveError{Reason: reasonUnknown, Suffix: RedactHandle(handle)}
}

// Counters returns a snapshot of the lifecycle counters.
func (s *SnapshotStore[P]) Counters() SnapshotCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counters
}

// expireLocked retires every live record whose absolute expiry has passed. An
// in_flight record is left alone: an open transaction owns it, and the TTL is far
// larger than any single dispatch, so it is retired by the transaction instead.
func (s *SnapshotStore[P]) expireLocked(now time.Time) {
	for h, rec := range s.live {
		if rec.Lifecycle == LifecycleInFlight {
			continue
		}
		if !now.Before(rec.ExpiresAt) {
			s.tombstoneLocked(h, reasonExpired, now)
			s.counters.Expired++
		}
	}
}

// evictOldestLocked tombstones the least-recently-created evictable (non
// in_flight) live record and reports whether it evicted one. An in_flight record
// is never evicted because an open transaction still needs it.
func (s *SnapshotStore[P]) evictOldestLocked(now time.Time) bool {
	var oldest *SnapshotRecord[P]
	for _, rec := range s.live {
		if rec.Lifecycle == LifecycleInFlight {
			continue
		}
		if oldest == nil || rec.CreatedAt.Before(oldest.CreatedAt) ||
			(rec.CreatedAt.Equal(oldest.CreatedAt) && rec.createSeq < oldest.createSeq) {
			oldest = rec
		}
	}
	if oldest == nil {
		return false
	}
	s.tombstoneLocked(oldest.Handle, reasonEvicted, now)
	s.counters.Evicted++
	return true
}

// tombstoneLocked releases a live record's payload, removes it from the live and
// per-target tables, and records a payload-free tombstone.
func (s *SnapshotStore[P]) tombstoneLocked(handle, reason string, now time.Time) {
	if rec, ok := s.live[handle]; ok {
		var zero P
		rec.Payload = zero
		switch reason {
		case reasonExpired:
			rec.Lifecycle = LifecycleExpired
		default:
			rec.Lifecycle = LifecycleSuperseded
		}
		delete(s.live, handle)
		if s.targetLive[rec.Target] == handle {
			delete(s.targetLive, rec.Target)
		}
	}
	s.addTombstoneLocked(handle, reason, now)
	s.logf(reason, handle)
}

// addTombstoneLocked inserts a tombstone under the FIFO capacity limit, dropping
// the least-recently-created tombstone when full.
func (s *SnapshotStore[P]) addTombstoneLocked(handle, reason string, now time.Time) {
	if _, exists := s.tombstones[handle]; exists {
		s.tombstones[handle] = tombstone{reason: reason, retiredAt: now}
		return
	}
	for len(s.tombstones) >= MaxTombstones {
		oldest := s.tombOrder[0]
		s.tombOrder = s.tombOrder[1:]
		delete(s.tombstones, oldest)
	}
	s.tombstones[handle] = tombstone{reason: reason, retiredAt: now}
	s.tombOrder = append(s.tombOrder, handle)
}

// lookupLiveLocked returns the live record whose handle matches in constant time,
// or nil. Comparing every stored handle avoids leaking which handles exist
// through comparison timing.
func (s *SnapshotStore[P]) lookupLiveLocked(handle string) *SnapshotRecord[P] {
	hb := []byte(handle)
	var found *SnapshotRecord[P]
	for h, rec := range s.live {
		if subtle.ConstantTimeCompare([]byte(h), hb) == 1 {
			found = rec
		}
	}
	return found
}

// lookupTombstoneLocked returns the tombstone reason for handle, matched in
// constant time.
func (s *SnapshotStore[P]) lookupTombstoneLocked(handle string) (string, bool) {
	hb := []byte(handle)
	reason := ""
	ok := false
	for h, tomb := range s.tombstones {
		if subtle.ConstantTimeCompare([]byte(h), hb) == 1 {
			reason = tomb.reason
			ok = true
		}
	}
	return reason, ok
}

// logf emits one redacted stderr line carrying the event, the redacted handle,
// and the current counters. It never writes the full handle.
func (s *SnapshotStore[P]) logf(event, handle string) {
	if s.logger == nil {
		return
	}
	c := s.counters
	s.logger(fmt.Sprintf("snapshot %s handle=%s minted=%d resolved=%d expired=%d stale=%d evicted=%d mismatched=%d concurrent=%d uncertain=%d refresh_failed=%d",
		event, RedactHandle(handle), c.Minted, c.Resolved, c.Expired, c.Stale, c.Evicted, c.Mismatched, c.Concurrent, c.Uncertain, c.RefreshFailed))
}

// ResolveError is a typed handle-resolution failure. Code maps the reason to the
// M2 snapshot tool-error taxonomy. The message and suffix are always redacted.
type ResolveError struct {
	Reason string
	Suffix string
}

func (e *ResolveError) Error() string {
	return "snapshot handle " + e.Reason + " " + e.Suffix
}

// Code maps the resolution reason to a snapshot tool-error code. Superseded is
// reported as stale; an evicted handle (dropped for capacity) is reported as
// expired since the caller must recapture either way.
func (e *ResolveError) Code() string {
	switch e.Reason {
	case reasonMalformed:
		return ErrSnapshotRefMalformed
	case reasonExpired:
		return ErrSnapshotRefExpired
	case reasonSuperseded:
		return ErrSnapshotRefStale
	case reasonEvicted:
		return ErrSnapshotRefExpired
	case reasonInUse:
		return ErrSnapshotRefInUse
	default:
		return ErrSnapshotRefUnknown
	}
}

// RedactHandle returns the loggable form of a handle: the fixed prefix and the
// final six characters only, never the middle. Correlation without disclosure.
func RedactHandle(h string) string {
	tail := h
	if len(h) > 6 {
		tail = h[len(h)-6:]
	}
	return HandlePrefix + "..." + tail
}

// ValidHandleFormat reports whether h has the pinned shape: the prefix followed
// by unpadded base64url of exactly TokenBytes bytes. The dispatcher uses it to
// separate a malformed handle from an unknown one before resolving.
func ValidHandleFormat(h string) bool {
	if !strings.HasPrefix(h, HandlePrefix) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(h[len(HandlePrefix):])
	return err == nil && len(b) == TokenBytes
}
