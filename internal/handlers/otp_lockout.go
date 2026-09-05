package handlers

import (
	"sync"
	"time"
)

// OTP lockout policy.
//
// A mistyped code should cost the user an attempt, not the form they typed it
// into, so wrong codes are counted instead of ending the login. Once a phone
// number spends its budget it goes on cooldown: both code verification and new
// code sends are refused for that number until the cooldown elapses.
//
// The per-IP budget is deliberately far looser than the per-phone one. Iranian
// mobile networks put many subscribers behind a single NAT address, so a tight
// per-IP lock would punish bystanders for one person's typos; it exists only to
// blunt an attacker cycling through phone numbers from a single host.
const (
	maxOTPFailuresPerPhone = 5
	maxOTPFailuresPerIP    = 15
	otpLockoutDuration     = 5 * time.Minute
	otpFailureWindow       = 15 * time.Minute
)

// attemptTracker counts failed OTP verifications per key (a phone number or a
// client IP) and locks a key out once it exceeds its budget. It is safe for
// concurrent use. Entries are lazily expired on access, with a sweep once the
// table grows large enough that memory growth could otherwise become unbounded
// (the same approach as RateLimiter).
type attemptTracker struct {
	mu      sync.Mutex
	now     func() time.Time // indirected so tests can advance the clock
	entries map[string]*attemptEntry
	// lastSweep throttles the full-map sweep (see RateLimiter): every fail()
	// past 10k keys must not pay O(n).
	lastSweep time.Time
}

type attemptEntry struct {
	failures  int
	forgetAt  time.Time // accumulated failures are dropped once this passes
	lockedTil time.Time // zero unless the key is on cooldown
}

func newAttemptTracker() *attemptTracker {
	return &attemptTracker{now: time.Now, entries: make(map[string]*attemptEntry)}
}

// lockedFor reports how long key remains on cooldown, or 0 if it is not locked.
func (t *attemptTracker) lockedFor(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[key]
	if !ok {
		return 0
	}
	now := t.now()
	if now.Before(e.lockedTil) {
		return e.lockedTil.Sub(now)
	}
	// Not locked: drop the entry entirely once its failures are also stale, so
	// a key that behaved does not linger in the table.
	if !e.forgetAt.After(now) {
		delete(t.entries, key)
	}
	return 0
}

// fail records one failed attempt against key. It returns the cooldown that was
// just imposed (0 while attempts remain) and how many attempts are still left
// before the budget is spent.
func (t *attemptTracker) fail(key string, budget int) (lock time.Duration, remaining int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	e, ok := t.entries[key]
	if !ok || !e.forgetAt.After(now) {
		e = &attemptEntry{}
		t.entries[key] = e
	}
	e.failures++
	e.forgetAt = now.Add(otpFailureWindow)
	t.sweep(now)

	if e.failures >= budget {
		// Start the cooldown and clear the counter, so the key gets a fresh
		// budget when the cooldown ends rather than locking again on the next
		// single mistake.
		e.failures = 0
		e.lockedTil = now.Add(otpLockoutDuration)
		e.forgetAt = e.lockedTil
		return otpLockoutDuration, 0
	}
	return 0, budget - e.failures
}

// reset forgets any recorded failures for key. It is called for the phone number
// after a successful login; the client IP is deliberately not reset, so one
// successful login cannot wipe the evidence of an attacker's earlier sweep.
func (t *attemptTracker) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// setNow replaces the tracker's clock. Used by tests to advance past a cooldown
// without sleeping.
func (t *attemptTracker) setNow(now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
}

// sweep drops finished entries once the table is large enough for the memory to
// matter. Throttled to one full scan per minute so a key-flood cannot turn
// every fail() into O(n) CPU-DoS. Callers must hold the mutex.
func (t *attemptTracker) sweep(now time.Time) {
	if len(t.entries) <= 10000 || now.Sub(t.lastSweep) <= time.Minute {
		return
	}
	t.lastSweep = now
	for k, e := range t.entries {
		if !e.forgetAt.After(now) && !now.Before(e.lockedTil) {
			delete(t.entries, k)
		}
	}
}

// otpLockRemaining reports how long OTP login is on cooldown for this phone
// number or client IP, whichever has longer to run, and 0 when neither is
// locked.
func (h *Handler) otpLockRemaining(phone, ip string) time.Duration {
	lock := h.otpAttempts.lockedFor("phone:" + phone)
	if ipLock := h.otpAttempts.lockedFor("ip:" + ip); ipLock > lock {
		lock = ipLock
	}
	return lock
}

// recordOTPFailure counts one wrong code against both the phone number and the
// client IP. It returns the cooldown now in force (0 if the user may try again)
// and how many attempts the phone number has left.
func (h *Handler) recordOTPFailure(phone, ip string) (lock time.Duration, remaining int) {
	lock, remaining = h.otpAttempts.fail("phone:"+phone, maxOTPFailuresPerPhone)
	if ipLock, _ := h.otpAttempts.fail("ip:"+ip, maxOTPFailuresPerIP); ipLock > lock {
		lock = ipLock
		remaining = 0
	}
	return lock, remaining
}
