package pool

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/monitor"
)

// sharedMemberState holds failure/blacklist state shared across all pool instances.
// This enables hybrid mode where pool and multi-port modes share the same node state.
type sharedMemberState struct {
	mu               sync.Mutex
	failures         int
	blacklisted      bool
	blacklistedUntil time.Time
	entry            atomic.Pointer[monitor.EntryHandle]
	active           atomic.Int32
}

// failureKind classifies a dial failure so the caller can pick the right
// recovery policy: a quota reset (429) usually needs far longer than a network
// blip, and neither should count toward the permanent-blacklist threshold.
type failureKind int

const (
	faultPermanent failureKind = iota
	faultTransient
	faultRateLimit
)

// defaultTransientCooldown is the fallback cooldown when the policy leaves it
// unset, preserving the historical hardcoded behaviour.
const defaultTransientCooldown = 60 * time.Second

// classifyFailure buckets err into one of the three failure kinds.
//
// Rate-limit markers are checked first: a 429 response body frequently also
// mentions "timeout" or "try again", and the longer rate-limit cooldown must
// win in that case. Everything unrecognised is treated as permanent, so real
// faults (handshake/cert/protocol failures, 404, …) still accumulate toward
// the blacklist threshold.
func classifyFailure(err error) failureKind {
	if err == nil {
		return faultPermanent
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "429"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(msg, "rate limit"),
		strings.Contains(msg, "ratelimit"),
		strings.Contains(msg, "rate-limit"):
		return faultRateLimit
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "reset by peer"),
		strings.Contains(msg, "temporarily"),
		strings.Contains(msg, "try again"),
		strings.Contains(msg, "service unavailable"),
		strings.Contains(msg, "503"):
		return faultTransient
	}
	return faultPermanent
}

// failurePolicy bundles the tunables recordFailure needs. The zero value is
// usable and behaves like the original hardcoded 60s cooldown.
type failurePolicy struct {
	Threshold         int
	BlacklistDuration time.Duration
	TransientCooldown time.Duration
	RateLimitCooldown time.Duration
}

// cooldownFor resolves the pause to apply for kind, filling in defaults
// defensively so an unset policy still behaves sanely. Permanent faults have
// no cooldown of their own — they go through the threshold/blacklist path.
func (p failurePolicy) cooldownFor(kind failureKind) time.Duration {
	transient := p.TransientCooldown
	if transient <= 0 {
		transient = defaultTransientCooldown
	}
	if kind == faultRateLimit {
		if p.RateLimitCooldown > 0 {
			return p.RateLimitCooldown
		}
		return transient
	}
	return transient
}

var sharedStateStore sync.Map // map[tag]*sharedMemberState

// acquireSharedState returns the shared state for a tag, creating if needed.
func acquireSharedState(tag string) *sharedMemberState {
	if v, ok := sharedStateStore.Load(tag); ok {
		return v.(*sharedMemberState)
	}
	state := &sharedMemberState{}
	actual, _ := sharedStateStore.LoadOrStore(tag, state)
	return actual.(*sharedMemberState)
}

// lookupSharedState returns the shared state if it exists.
func lookupSharedState(tag string) (*sharedMemberState, bool) {
	v, ok := sharedStateStore.Load(tag)
	if !ok {
		return nil, false
	}
	return v.(*sharedMemberState), true
}

// ResetSharedStateStore clears all shared state (used during config reload).
func ResetSharedStateStore() {
	sharedStateStore.Range(func(key, _ any) bool {
		sharedStateStore.Delete(key)
		return true
	})
	ResetDialerRegistry()
}

func (s *sharedMemberState) attachEntry(entry *monitor.EntryHandle) {
	if entry == nil {
		return
	}
	s.entry.Store(entry)
}

func (s *sharedMemberState) entryHandle() *monitor.EntryHandle {
	return s.entry.Load()
}

// recordFailure records a failure and decides whether to blacklist the node.
//
// Rate-limit (429) and other transient errors (timeouts, connection resets, 503)
// do NOT count toward the permanent threshold; instead they impose a cooldown —
// sized per kind by policy — so the node is briefly skipped and then retried
// automatically. Permanent errors (handshake/cert/protocol failures, 404, etc.)
// accumulate toward the threshold and trigger the full blacklist duration once
// it is reached.
//
// Returns: (current permanent-failure count, blacklisted, effective until, kind).
func (s *sharedMemberState) recordFailure(cause error, policy failurePolicy) (int, bool, time.Time, failureKind) {
	kind := classifyFailure(cause)

	s.mu.Lock()
	var count int
	triggered := false
	var until time.Time
	if kind == faultPermanent {
		s.failures++
		count = s.failures
		if s.failures >= policy.Threshold {
			triggered = true
			until = time.Now().Add(policy.BlacklistDuration)
			s.failures = 0
		}
	} else {
		// Cooldown only; do not accumulate toward the long blacklist.
		count = s.failures
		until = time.Now().Add(policy.cooldownFor(kind))
	}
	if !until.IsZero() {
		// Never let a short cooldown cut an already-running longer ban short:
		// a 60s transient blip must not release a node mid-way through a 24h
		// permanent blacklist. Keep whichever expiry is later.
		if s.blacklisted && s.blacklistedUntil.After(until) {
			until = s.blacklistedUntil
		}
		s.blacklisted = true
		s.blacklistedUntil = until
	}
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.RecordFailure(cause)
		if !until.IsZero() {
			entry.Blacklist(until)
		}
	}
	return count, triggered, until, kind
}

func (s *sharedMemberState) recordSuccess() {
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.RecordSuccess()
	}
}

// isBlacklisted checks if the node is currently blacklisted, auto-clearing if expired.
func (s *sharedMemberState) isBlacklisted(now time.Time) bool {
	s.mu.Lock()
	expired := s.blacklisted && now.After(s.blacklistedUntil)
	if expired {
		s.blacklisted = false
		s.blacklistedUntil = time.Time{}
	}
	blacklisted := s.blacklisted
	s.mu.Unlock()

	if expired {
		if entry := s.entry.Load(); entry != nil {
			entry.ClearBlacklist()
		}
	}
	return blacklisted
}

// blacklistRemaining returns the remaining blacklist duration.
// Returns 0 if not blacklisted.
func (s *sharedMemberState) blacklistRemaining(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.blacklisted {
		return 0
	}

	remaining := s.blacklistedUntil.Sub(now)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (s *sharedMemberState) forceRelease() {
	s.mu.Lock()
	s.failures = 0
	s.blacklisted = false
	s.blacklistedUntil = time.Time{}
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.ClearBlacklist()
	}
}

func (s *sharedMemberState) incActive() {
	s.active.Add(1)
	if entry := s.entry.Load(); entry != nil {
		entry.IncActive()
	}
}

func (s *sharedMemberState) decActive() {
	s.active.Add(-1)
	if entry := s.entry.Load(); entry != nil {
		entry.DecActive()
	}
}

func (s *sharedMemberState) activeCount() int32 {
	return s.active.Load()
}

// releaseSharedMember clears blacklist state for a tag (called from release functions).
func releaseSharedMember(tag string) {
	if state, ok := lookupSharedState(tag); ok {
		state.forceRelease()
	}
}

// blacklistSharedMember manually blacklists a node in pool shared state.
func blacklistSharedMember(tag string, duration time.Duration) {
	if state, ok := lookupSharedState(tag); ok {
		until := time.Now().Add(duration)
		state.mu.Lock()
		state.blacklisted = true
		state.blacklistedUntil = until
		state.failures = 0
		state.mu.Unlock()
	}
}
