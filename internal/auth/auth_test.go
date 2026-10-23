package auth

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const key = "s3cret-key-value"

func TestDisabledWhenNoKey(t *testing.T) {
	a := New("", time.Hour)
	if a.Enabled() {
		t.Error("empty key should disable auth")
	}
	if !a.VerifyKey("anything") {
		t.Error("disabled auth should accept any key")
	}
	if !a.VerifyToken("garbage", time.Now()) {
		t.Error("disabled auth should accept any token")
	}
	if !strings.Contains(a.Describe(), "DISABLED") {
		t.Errorf("startup line should be loud about being open: %q", a.Describe())
	}
}

func TestVerifyKey(t *testing.T) {
	a := New(key, time.Hour)
	if !a.VerifyKey(key) {
		t.Error("correct key rejected")
	}
	for _, bad := range []string{"", "wrong", key + "x", key[:len(key)-1], strings.ToUpper(key)} {
		if a.VerifyKey(bad) {
			t.Errorf("bad key accepted: %q", bad)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	a := New(key, time.Hour)
	now := time.Now()
	tok, exp := a.Issue(now)
	if !a.VerifyToken(tok, now) {
		t.Fatal("freshly issued token rejected")
	}
	if exp.Before(now) {
		t.Error("expiry should be in the future")
	}
	// The token must not contain the key itself: the cookie jar should never hold
	// the master credential.
	if strings.Contains(tok, key) {
		t.Error("token leaks the API key")
	}
}

func TestTokenExpires(t *testing.T) {
	a := New(key, time.Hour)
	now := time.Now()
	tok, _ := a.Issue(now)
	if a.VerifyToken(tok, now.Add(2*time.Hour)) {
		t.Error("expired token accepted")
	}
	if !a.VerifyToken(tok, now.Add(59*time.Minute)) {
		t.Error("token rejected before its expiry")
	}
}

// The expiry is inside the signed payload, so a client cannot extend its own
// session by editing the cookie.
func TestExpiryCannotBeForged(t *testing.T) {
	a := New(key, time.Hour)
	now := time.Now()
	tok, _ := a.Issue(now)
	_, sig, _ := strings.Cut(tok, ".")
	far := strconv.FormatInt(now.Add(100*time.Hour).Unix(), 10)
	if a.VerifyToken(far+"."+sig, now) {
		t.Error("token with a rewritten expiry was accepted")
	}
}

func TestTokenGarbageRejected(t *testing.T) {
	a := New(key, time.Hour)
	now := time.Now()
	for _, bad := range []string{"", ".", "abc", "abc.def", "0.", ".sig", strconv.FormatInt(now.Unix()+3600, 10)} {
		if a.VerifyToken(bad, now) {
			t.Errorf("garbage token accepted: %q", bad)
		}
	}
}

// Changing the key must invalidate outstanding sessions — that's the revocation
// story, since there is no server-side session list to clear.
func TestRotatingKeyRevokesSessions(t *testing.T) {
	old := New(key, time.Hour)
	now := time.Now()
	tok, _ := old.Issue(now)
	rotated := New("a-different-key", time.Hour)
	if rotated.VerifyToken(tok, now) {
		t.Error("session survived a key change")
	}
}

func TestRateLimit(t *testing.T) {
	a := New(key, time.Hour)
	now := time.Now()
	const ip = "192.168.1.50"
	for i := 0; i < maxAttempts; i++ {
		if !a.Attempt(ip, now) {
			t.Fatalf("attempt %d blocked too early", i+1)
		}
	}
	if a.Attempt(ip, now) {
		t.Error("attempts past the limit should be refused")
	}
	// A different client is unaffected.
	if !a.Attempt("192.168.1.51", now) {
		t.Error("one client's failures blocked another")
	}
	// The window rolls over.
	if !a.Attempt(ip, now.Add(2*attemptWin)) {
		t.Error("limit should lift after the window")
	}
	// Success clears the record.
	for i := 0; i < maxAttempts; i++ {
		a.Attempt(ip, now.Add(2*attemptWin))
	}
	a.RecordSuccess(ip)
	if !a.Attempt(ip, now.Add(2*attemptWin)) {
		t.Error("successful login should clear the failure count")
	}
}

// Parallel guesses must not all slip past the check before any is counted.
func TestRateLimitHoldsUnderConcurrency(t *testing.T) {
	a := New(key, time.Hour)
	now := time.Now()
	var wg sync.WaitGroup
	var allowed atomic.Int32
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.Attempt("10.0.0.9", now) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != maxAttempts {
		t.Errorf("%d parallel attempts allowed, want exactly %d", got, maxAttempts)
	}
}

func TestShortKeyWarns(t *testing.T) {
	if d := New("hunter2", time.Hour).Describe(); !strings.Contains(d, "WARNING") {
		t.Errorf("a 7-character key should draw a warning: %q", d)
	}
	if d := New(strings.Repeat("k", MinKeyLen), time.Hour).Describe(); strings.Contains(d, "WARNING") {
		t.Errorf("a %d-character key should not warn: %q", MinKeyLen, d)
	}
}

func TestKeyFromRequest(t *testing.T) {
	for _, tc := range []struct {
		name, header, value, want string
	}{
		{"x-api-key", "X-API-Key", key, key},
		{"bearer", "Authorization", "Bearer " + key, key},
		{"bearer lowercase scheme", "Authorization", "bearer " + key, key},
		{"bearer padded", "Authorization", "Bearer   " + key + "  ", key},
		{"basic is not accepted", "Authorization", "Basic " + key, ""},
		{"empty", "X-API-Key", "", ""},
	} {
		r, _ := http.NewRequest("GET", "/api/items", nil)
		if tc.value != "" {
			r.Header.Set(tc.header, tc.value)
		}
		if got := KeyFromRequest(r); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClientIgnoresForwardedFor(t *testing.T) {
	r, _ := http.NewRequest("GET", "/api/items", nil)
	r.RemoteAddr = "10.0.0.5:44321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := Client(r); got != "10.0.0.5" {
		// Honouring the header would let one client mint unlimited rate-limit
		// buckets simply by varying it.
		t.Errorf("Client() = %q, want the peer address 10.0.0.5", got)
	}
}

// One IPv6 host can use any address in its /64, so those must share a bucket.
func TestClientGroupsIPv6By64(t *testing.T) {
	r1, _ := http.NewRequest("GET", "/", nil)
	r1.RemoteAddr = "[2001:db8:1:2::1]:5000"
	r2, _ := http.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "[2001:db8:1:2:ffff:ffff:ffff:fffe]:5001"
	r3, _ := http.NewRequest("GET", "/", nil)
	r3.RemoteAddr = "[2001:db8:1:3::1]:5000"
	if Client(r1) != Client(r2) {
		t.Errorf("same /64 got different buckets: %q vs %q", Client(r1), Client(r2))
	}
	if Client(r1) == Client(r3) {
		t.Errorf("different /64s share a bucket: %q", Client(r1))
	}
	r4, _ := http.NewRequest("GET", "/", nil)
	r4.RemoteAddr = "[::ffff:10.0.0.5]:80"
	if got := Client(r4); got != "::ffff:10.0.0.5" {
		t.Errorf("IPv4-mapped address should stay per-host, got %q", got)
	}
}
