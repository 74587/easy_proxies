package pool

import (
	"errors"
	"testing"
	"time"
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
