package pool

import (
	"errors"
	"fmt"
	"testing"
	"time"

	singlog "github.com/sagernet/sing-box/log"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want failureKind
	}{
		{"nil error is permanent", nil, faultPermanent},
		{"bare 429", errors.New("unexpected status 429"), faultRateLimit},
		{"too many requests", errors.New("HTTP 400: Too Many Requests"), faultRateLimit},
		{"rate limit phrase", errors.New("upstream rate limit exceeded"), faultRateLimit},
		{"ratelimit one word", errors.New("RateLimit quota exhausted"), faultRateLimit},
		{"hyphenated rate-limit", errors.New("node is rate-limited"), faultRateLimit},
		{"io timeout", errors.New("dial tcp 10.0.0.1:443: i/o timeout"), faultTransient},
		{"deadline exceeded", errors.New("context deadline exceeded"), faultTransient},
		{"connection reset", errors.New("read: connection reset by peer"), faultTransient},
		{"503", errors.New("503 service unavailable"), faultTransient},
		{"try again", errors.New("temporary failure, try again"), faultTransient},
		{"tls handshake", errors.New("tls: handshake failure"), faultPermanent},
		{"404", errors.New("404 not found"), faultPermanent},
		{"unknown", errors.New("no route to host"), faultPermanent},
		// A 429 body often also mentions a timeout; the longer rate-limit
		// cooldown must win so we do not hammer an exhausted quota.
		{"429 wins over timeout", errors.New("429 too many requests: timeout, try again later"), faultRateLimit},
		// Status codes must not be matched as bare substrings: an ephemeral port
		// or a dotted quad can contain "429"/"503" without being a status code.
		{"port containing 429 is transient", errors.New("dial tcp 203.0.113.5:44290: i/o timeout"), faultTransient},
		{"port containing 503 is transient via reset", errors.New("read tcp 10.0.0.1:45030: connection reset by peer"), faultTransient},
		{"429 as a port is not a rate limit", errors.New("dial tcp 10.0.0.1:429: connect: connection refused"), faultPermanent},
		{"429 followed by colon", errors.New("error 429: slow down"), faultRateLimit},
		{"429 in a status line", errors.New("HTTP/1.1 429 Too Many Requests"), faultRateLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.err); got != tc.want {
				t.Fatalf("classifyFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestFailurePolicyCooldownFor(t *testing.T) {
	cases := []struct {
		name   string
		policy failurePolicy
		kind   failureKind
		want   time.Duration
	}{
		{
			name:   "zero value falls back to 60s",
			policy: failurePolicy{},
			kind:   faultTransient,
			want:   defaultTransientCooldown,
		},
		{
			name:   "zero value rate-limit falls back to transient default",
			policy: failurePolicy{},
			kind:   faultRateLimit,
			want:   defaultTransientCooldown,
		},
		{
			name:   "unset rate-limit falls back to configured transient",
			policy: failurePolicy{TransientCooldown: 90 * time.Second},
			kind:   faultRateLimit,
			want:   90 * time.Second,
		},
		{
			name:   "explicit rate-limit wins",
			policy: failurePolicy{TransientCooldown: 60 * time.Second, RateLimitCooldown: 2 * time.Hour},
			kind:   faultRateLimit,
			want:   2 * time.Hour,
		},
		{
			name:   "transient unaffected by rate-limit setting",
			policy: failurePolicy{TransientCooldown: 60 * time.Second, RateLimitCooldown: 2 * time.Hour},
			kind:   faultTransient,
			want:   60 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.cooldownFor(tc.kind); got != tc.want {
				t.Fatalf("cooldownFor(%v) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// approxUntil asserts that until lands within a small tolerance of now+want,
// absorbing the clock read inside recordFailure.
func approxUntil(t *testing.T, start time.Time, until time.Time, want time.Duration) {
	t.Helper()
	got := until.Sub(start)
	const tolerance = 5 * time.Second
	if got < want-tolerance || got > want+tolerance {
		t.Fatalf("cooldown = %v, want ~%v", got, want)
	}
}

func TestRecordFailureRateLimitUsesRateLimitCooldown(t *testing.T) {
	policy := failurePolicy{
		Threshold:         3,
		BlacklistDuration: 24 * time.Hour,
		TransientCooldown: 60 * time.Second,
		RateLimitCooldown: 2 * time.Hour,
	}
	s := &sharedMemberState{}

	start := time.Now()
	count, blacklisted, until, kind := s.recordFailure(errors.New("429 too many requests"), policy)

	if kind != faultRateLimit {
		t.Fatalf("kind = %v, want faultRateLimit", kind)
	}
	if blacklisted {
		t.Fatal("rate-limit must not trigger the permanent blacklist")
	}
	if count != 0 {
		t.Fatalf("permanent failure count = %d, want 0", count)
	}
	approxUntil(t, start, until, 2*time.Hour)
	if !s.isBlacklisted(time.Now()) {
		t.Fatal("node should be cooling down")
	}
}

func TestRecordFailureTransientUsesTransientCooldown(t *testing.T) {
	policy := failurePolicy{
		Threshold:         3,
		BlacklistDuration: 24 * time.Hour,
		TransientCooldown: 60 * time.Second,
		RateLimitCooldown: 2 * time.Hour,
	}
	s := &sharedMemberState{}

	start := time.Now()
	count, blacklisted, until, kind := s.recordFailure(errors.New("dial tcp: i/o timeout"), policy)

	if kind != faultTransient {
		t.Fatalf("kind = %v, want faultTransient", kind)
	}
	if blacklisted {
		t.Fatal("transient must not trigger the permanent blacklist")
	}
	if count != 0 {
		t.Fatalf("permanent failure count = %d, want 0", count)
	}
	approxUntil(t, start, until, 60*time.Second)
}

func TestRecordFailureCooldownsNeverReachThreshold(t *testing.T) {
	policy := failurePolicy{
		Threshold:         3,
		BlacklistDuration: 24 * time.Hour,
		TransientCooldown: 60 * time.Second,
		RateLimitCooldown: 2 * time.Hour,
	}

	for _, cause := range []error{
		errors.New("429 too many requests"),
		errors.New("i/o timeout"),
	} {
		s := &sharedMemberState{}
		for i := 0; i < policy.Threshold+2; i++ {
			count, blacklisted, _, _ := s.recordFailure(cause, policy)
			if blacklisted {
				t.Fatalf("%v: blacklisted on hit %d, want never", cause, i+1)
			}
			if count != 0 {
				t.Fatalf("%v: permanent count = %d on hit %d, want 0", cause, count, i+1)
			}
		}
	}
}

func TestRecordFailurePermanentAccumulatesAndBlacklists(t *testing.T) {
	policy := failurePolicy{
		Threshold:         3,
		BlacklistDuration: 24 * time.Hour,
		TransientCooldown: 60 * time.Second,
		RateLimitCooldown: 2 * time.Hour,
	}
	s := &sharedMemberState{}
	cause := errors.New("tls: handshake failure")

	for i := 1; i < policy.Threshold; i++ {
		count, blacklisted, until, kind := s.recordFailure(cause, policy)
		if kind != faultPermanent {
			t.Fatalf("hit %d: kind = %v, want faultPermanent", i, kind)
		}
		if blacklisted {
			t.Fatalf("hit %d: blacklisted before threshold", i)
		}
		if count != i {
			t.Fatalf("hit %d: count = %d, want %d", i, count, i)
		}
		if !until.IsZero() {
			t.Fatalf("hit %d: until = %v, want zero before threshold", i, until)
		}
	}

	start := time.Now()
	_, blacklisted, until, kind := s.recordFailure(cause, policy)
	if kind != faultPermanent {
		t.Fatalf("kind = %v, want faultPermanent", kind)
	}
	if !blacklisted {
		t.Fatal("threshold reached but not blacklisted")
	}
	approxUntil(t, start, until, 24*time.Hour)
}

func TestRecordFailureZeroPolicyKeepsLegacyDefaults(t *testing.T) {
	// A zero-value policy must behave like the original hardcoded 60s cooldown,
	// with rate-limit inheriting the same fallback.
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"transient", errors.New("i/o timeout")},
		{"rate limit", errors.New("429 too many requests")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sharedMemberState{}
			start := time.Now()
			_, _, until, _ := s.recordFailure(tc.cause, failurePolicy{})
			approxUntil(t, start, until, defaultTransientCooldown)
		})
	}
}

func TestRecordFailureTransientDoesNotShortenLongerBlacklist(t *testing.T) {
	policy := failurePolicy{
		Threshold:         1,
		BlacklistDuration: 24 * time.Hour,
		TransientCooldown: 60 * time.Second,
		RateLimitCooldown: 2 * time.Hour,
	}
	s := &sharedMemberState{}

	// Threshold of 1 blacklists on the first permanent fault.
	start := time.Now()
	_, blacklisted, banUntil, _ := s.recordFailure(errors.New("tls: handshake failure"), policy)
	if !blacklisted {
		t.Fatal("expected immediate blacklist at threshold 1")
	}
	approxUntil(t, start, banUntil, 24*time.Hour)

	// A 60s transient blip must not release the node 24h early.
	_, _, until, kind := s.recordFailure(errors.New("i/o timeout"), policy)
	if kind != faultTransient {
		t.Fatalf("kind = %v, want faultTransient", kind)
	}
	if until.Before(banUntil) {
		t.Fatalf("transient cooldown shortened the ban: until = %v, ban = %v", until, banUntil)
	}

	s.mu.Lock()
	stored := s.blacklistedUntil
	s.mu.Unlock()
	if stored.Before(banUntil) {
		t.Fatalf("stored blacklistedUntil shortened: %v < %v", stored, banUntil)
	}
}

// TestStatusCodeRegexpBoundaries pins the boundary behaviour directly, so a
// classification that happens to be right for the wrong reason (e.g. an error
// with a 503-shaped port that also says "connection reset") still fails here if
// the regexp regresses to a bare substring match.
func TestStatusCodeRegexpBoundaries(t *testing.T) {
	cases := []struct {
		name          string
		msg           string
		wantRateLimit bool
		wantUnavail   bool
	}{
		{"ephemeral port 44290", "dial tcp 203.0.113.5:44290: i/o timeout", false, false},
		{"ephemeral port 45030", "read tcp 10.0.0.1:45030: connection reset by peer", false, false},
		{"429 as port", "dial tcp 10.0.0.1:429: connect: connection refused", false, false},
		{"503 as port", "dial tcp 10.0.0.1:503: connect: connection refused", false, false},
		{"dotted quad 4.429", "dial tcp 4.429.1.1:80: no route to host", false, false},
		{"real 429 at end", "unexpected status 429", true, false},
		{"real 429 with colon", "error 429: slow down", true, false},
		{"real 429 in status line", "http/1.1 429 too many requests", true, false},
		{"real 503 at start", "503 service unavailable", false, true},
		{"longer code 4291", "unexpected status 4291", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateLimitCodeRe.MatchString(tc.msg); got != tc.wantRateLimit {
				t.Fatalf("rateLimitCodeRe.MatchString(%q) = %v, want %v", tc.msg, got, tc.wantRateLimit)
			}
			if got := serviceUnavailCodeRe.MatchString(tc.msg); got != tc.wantUnavail {
				t.Fatalf("serviceUnavailCodeRe.MatchString(%q) = %v, want %v", tc.msg, got, tc.wantUnavail)
			}
		})
	}
}

// TestRecordFailurePortLikeErrorAccumulatesToThreshold is the regression for the
// substring collision: a permanent fault whose message merely contains a port
// like 44290 used to be classified as a rate limit, and because cooldowns never
// increment s.failures such a node could never reach the blacklist threshold.
func TestRecordFailurePortLikeErrorAccumulatesToThreshold(t *testing.T) {
	policy := failurePolicy{
		Threshold:         3,
		BlacklistDuration: 24 * time.Hour,
		TransientCooldown: 60 * time.Second,
		RateLimitCooldown: 2 * time.Hour,
	}
	s := &sharedMemberState{}
	cause := errors.New("dial tcp 203.0.113.5:44290: tls: handshake failure")

	for i := 1; i < policy.Threshold; i++ {
		count, blacklisted, _, kind := s.recordFailure(cause, policy)
		if kind != faultPermanent {
			t.Fatalf("hit %d: kind = %v, want faultPermanent", i, kind)
		}
		if blacklisted {
			t.Fatalf("hit %d: blacklisted before threshold", i)
		}
		if count != i {
			t.Fatalf("hit %d: count = %d, want %d", i, count, i)
		}
	}

	_, blacklisted, _, _ := s.recordFailure(cause, policy)
	if !blacklisted {
		t.Fatal("threshold reached but not blacklisted")
	}
	if banIsCooldown(s) {
		t.Fatal("threshold blacklist must not be marked as a cooldown")
	}
}

// banIsCooldown reads the flag under the state lock.
func banIsCooldown(s *sharedMemberState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.banIsCooldown
}

// cooldownPolicy is the shared policy for the release tests: a 2h rate-limit
// cooldown is long enough to be unambiguously "live" during the test run.
var cooldownPolicy = failurePolicy{
	Threshold:         1,
	BlacklistDuration: 24 * time.Hour,
	TransientCooldown: 60 * time.Second,
	RateLimitCooldown: 2 * time.Hour,
}

func TestReleaseIfPermanentPreservesLiveCooldown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  time.Duration
	}{
		{"rate limit", errors.New("429 too many requests"), 2 * time.Hour},
		{"transient", errors.New("i/o timeout"), 60 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sharedMemberState{}
			_, _, until, _ := s.recordFailure(tc.cause, cooldownPolicy)
			if !banIsCooldown(s) {
				t.Fatal("ban should be flagged as a cooldown")
			}

			if s.releaseIfPermanent(time.Now()) {
				t.Fatal("releaseIfPermanent cleared a live cooldown")
			}
			if !s.isBlacklisted(time.Now()) {
				t.Fatal("node must stay parked for the remainder of the cooldown")
			}
			if !banIsCooldown(s) {
				t.Fatal("cooldown flag was cleared")
			}
			s.mu.Lock()
			stored := s.blacklistedUntil
			s.mu.Unlock()
			if !stored.Equal(until) {
				t.Fatalf("blacklistedUntil changed: %v, want %v", stored, until)
			}
		})
	}
}

func TestReleaseIfPermanentClearsExpiredCooldown(t *testing.T) {
	s := &sharedMemberState{}
	_, _, until, _ := s.recordFailure(errors.New("429 too many requests"), cooldownPolicy)

	if !s.releaseIfPermanent(until.Add(time.Second)) {
		t.Fatal("releaseIfPermanent should clear an expired cooldown")
	}
	if s.isBlacklisted(time.Now()) {
		t.Fatal("node still blacklisted after release")
	}
	if banIsCooldown(s) {
		t.Fatal("cooldown flag not reset")
	}
}

// TestReleaseIfPermanentClearsThresholdBlacklist guards the #8/#9 fix: a stale
// permanent ban must still be auto-cleared by a successful probe / the
// all-blacklisted fallback, otherwise a 24h ban can wedge a pool.
func TestReleaseIfPermanentClearsThresholdBlacklist(t *testing.T) {
	s := &sharedMemberState{}
	_, blacklisted, _, _ := s.recordFailure(errors.New("tls: handshake failure"), cooldownPolicy)
	if !blacklisted {
		t.Fatal("expected immediate blacklist at threshold 1")
	}

	if !s.releaseIfPermanent(time.Now()) {
		t.Fatal("releaseIfPermanent must clear a permanent blacklist")
	}
	if s.isBlacklisted(time.Now()) {
		t.Fatal("node still blacklisted after release")
	}
	s.mu.Lock()
	failures := s.failures
	s.mu.Unlock()
	if failures != 0 {
		t.Fatalf("failures = %d, want 0", failures)
	}
}

// TestForceReleaseClearsLiveCooldown covers the manual WebUI path, which stays
// unconditional: an operator asking for a node back always gets it back.
func TestForceReleaseClearsLiveCooldown(t *testing.T) {
	s := &sharedMemberState{}
	s.recordFailure(errors.New("429 too many requests"), cooldownPolicy)
	if !s.isBlacklisted(time.Now()) {
		t.Fatal("node should be cooling down")
	}

	s.forceRelease()
	if s.isBlacklisted(time.Now()) {
		t.Fatal("forceRelease left the cooldown in place")
	}
	if banIsCooldown(s) {
		t.Fatal("forceRelease did not reset the cooldown flag")
	}
}

// TestBanIsCooldownFollowsSurvivingBan checks that when recordFailure keeps the
// later of two expiries, the flag describes the ban that actually survived.
func TestBanIsCooldownFollowsSurvivingBan(t *testing.T) {
	t.Run("longer cooldown supersedes permanent ban", func(t *testing.T) {
		policy := failurePolicy{
			Threshold:         1,
			BlacklistDuration: time.Second,
			TransientCooldown: 60 * time.Second,
			RateLimitCooldown: 2 * time.Hour,
		}
		s := &sharedMemberState{}
		if _, blacklisted, _, _ := s.recordFailure(errors.New("tls: handshake failure"), policy); !blacklisted {
			t.Fatal("expected immediate blacklist at threshold 1")
		}
		if banIsCooldown(s) {
			t.Fatal("threshold blacklist must not be flagged as a cooldown")
		}

		// The 2h rate-limit cooldown outlives the 1s ban, so it takes over.
		s.recordFailure(errors.New("429 too many requests"), policy)
		if !banIsCooldown(s) {
			t.Fatal("the surviving rate-limit cooldown should be flagged as a cooldown")
		}
		if s.releaseIfPermanent(time.Now()) {
			t.Fatal("releaseIfPermanent cleared the surviving cooldown")
		}
	})

	t.Run("longer permanent ban survives a short cooldown", func(t *testing.T) {
		s := &sharedMemberState{}
		if _, blacklisted, _, _ := s.recordFailure(errors.New("tls: handshake failure"), cooldownPolicy); !blacklisted {
			t.Fatal("expected immediate blacklist at threshold 1")
		}

		// A 60s transient blip cannot shorten the running 24h ban, so the ban
		// keeps its permanent nature and stays auto-releasable.
		s.recordFailure(errors.New("i/o timeout"), cooldownPolicy)
		if banIsCooldown(s) {
			t.Fatal("the surviving 24h ban must keep its permanent nature")
		}
		if !s.releaseIfPermanent(time.Now()) {
			t.Fatal("releaseIfPermanent must still clear the surviving permanent ban")
		}
	})
}

// newTestPool builds the minimal poolOutbound that releaseIfAllBlacklistedLocked
// needs: it only reads p.members and logs, so the outbound adapters can be nil.
func newTestPool(states ...*sharedMemberState) *poolOutbound {
	p := &poolOutbound{logger: singlog.NewNOPFactory().Logger()}
	for i, state := range states {
		p.members = append(p.members, &memberState{
			tag:    fmt.Sprintf("node-%d", i),
			shared: state,
		})
	}
	return p
}

// TestReleaseIfAllBlacklistedLockedRespectsCooldowns is the pool-level
// regression: with one pool per node (multi-port / hybrid mode) "all members
// blacklisted" is trivially true the instant the only member cools down, so
// this path used to force-release the cooldown on the very next request.
func TestReleaseIfAllBlacklistedLockedRespectsCooldowns(t *testing.T) {
	rateLimited := errors.New("429 too many requests")
	permanent := errors.New("tls: handshake failure")

	t.Run("single-member pool keeps its cooldown", func(t *testing.T) {
		s := &sharedMemberState{}
		s.recordFailure(rateLimited, cooldownPolicy)
		p := newTestPool(s)

		if p.releaseIfAllBlacklistedLocked(time.Now()) {
			t.Fatal("released a live cooldown")
		}
		if !s.isBlacklisted(time.Now()) {
			t.Fatal("member should still be parked")
		}
	})

	t.Run("all permanent bans are still released", func(t *testing.T) {
		a, b := &sharedMemberState{}, &sharedMemberState{}
		a.recordFailure(permanent, cooldownPolicy)
		b.recordFailure(permanent, cooldownPolicy)
		p := newTestPool(a, b)

		if !p.releaseIfAllBlacklistedLocked(time.Now()) {
			t.Fatal("permanent bans must still be auto-released")
		}
		if a.isBlacklisted(time.Now()) || b.isBlacklisted(time.Now()) {
			t.Fatal("members still blacklisted after release")
		}
	})

	t.Run("mixed pool frees only the permanent ban", func(t *testing.T) {
		cooling, banned := &sharedMemberState{}, &sharedMemberState{}
		cooling.recordFailure(rateLimited, cooldownPolicy)
		banned.recordFailure(permanent, cooldownPolicy)
		p := newTestPool(cooling, banned)

		if !p.releaseIfAllBlacklistedLocked(time.Now()) {
			t.Fatal("expected the permanent ban to be released")
		}
		if banned.isBlacklisted(time.Now()) {
			t.Fatal("permanently banned member was not released")
		}
		if !cooling.isBlacklisted(time.Now()) {
			t.Fatal("rate-limited member should have stayed parked")
		}
	})

	t.Run("no release while a healthy member remains", func(t *testing.T) {
		cooling, healthy := &sharedMemberState{}, &sharedMemberState{}
		cooling.recordFailure(rateLimited, cooldownPolicy)
		p := newTestPool(cooling, healthy)

		if p.releaseIfAllBlacklistedLocked(time.Now()) {
			t.Fatal("released while a healthy member was available")
		}
		if !cooling.isBlacklisted(time.Now()) {
			t.Fatal("cooling member should still be parked")
		}
	})
}
