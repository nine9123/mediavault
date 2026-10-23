// Package auth gates the HTTP API behind a single shared API key — no accounts,
// no user table. It exists to stop someone who joins your network from simply
// opening the URL.
//
// Why a cookie and not just a header: the browser loads thumbnails, previews and
// video through <img src> and <video src>. Those requests are issued by the
// browser's own loader, and JavaScript cannot attach an Authorization or
// X-API-Key header to them. A header-only scheme would authenticate the JSON
// endpoints and 401 every piece of media. Routing media through fetch() into
// blob URLs would "fix" that and break video seeking instead, because it defeats
// HTTP Range requests. A cookie is the only mechanism the browser attaches to
// all three automatically. The header is still accepted, for curl and scripts.
//
// The session cookie is a signed, expiring token rather than the key itself, so
// the cookie jar never holds the master credential, expiry is enforced by the
// server rather than trusted from the client, and changing API_KEY immediately
// invalidates every outstanding session.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth verifies API keys and issues session tokens. A zero key means disabled.
type Auth struct {
	key string
	ttl time.Duration

	mu       sync.Mutex
	attempts map[string]*window // failed logins per client, for rate limiting
}

// New returns an Auth for the given key. An empty key disables authentication
// entirely (Enabled reports false and Allow lets everything through).
func New(key string, ttl time.Duration) *Auth {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return &Auth{key: key, ttl: ttl, attempts: map[string]*window{}}
}

// Enabled reports whether a key is configured.
func (a *Auth) Enabled() bool { return a.key != "" }

// TTL is how long an issued session lasts.
func (a *Auth) TTL() time.Duration { return a.ttl }

// VerifyKey checks a presented key against the configured one in constant time.
// Both sides are hashed first so the comparison is over fixed-length input and
// the key's length isn't observable through timing.
func (a *Auth) VerifyKey(given string) bool {
	if !a.Enabled() {
		return true
	}
	want := sha256.Sum256([]byte(a.key))
	got := sha256.Sum256([]byte(given))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// Issue mints a session token valid for TTL from now. The format is
// "<expiryUnix>.<base64url(HMAC-SHA256(key, expiryUnix))>" — stateless, so it
// survives a container restart, and self-describing, so expiry can be enforced
// without storing anything.
func (a *Auth) Issue(now time.Time) (token string, expires time.Time) {
	expires = now.Add(a.ttl)
	stamp := strconv.FormatInt(expires.Unix(), 10)
	return stamp + "." + a.sign(stamp), expires
}

func (a *Auth) sign(msg string) string {
	m := hmac.New(sha256.New, []byte(a.key))
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// VerifyToken checks a session token's signature and expiry.
func (a *Auth) VerifyToken(token string, now time.Time) bool {
	if !a.Enabled() {
		return true
	}
	stamp, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	// Signature first: an unsigned token's expiry claim is meaningless, and
	// checking in this order keeps the work constant for garbage input.
	if subtle.ConstantTimeCompare([]byte(a.sign(stamp)), []byte(sig)) != 1 {
		return false
	}
	unix, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return false
	}
	return now.Before(time.Unix(unix, 0))
}

// ---- login rate limiting ----

const (
	maxAttempts  = 10          // failed logins per client per window
	attemptWin   = time.Minute // the window
	maxTrackedIP = 4096        // cap so the map can't grow without bound
)

type window struct {
	count int
	start time.Time
}

// Attempt reports whether this client may try a key right now, and if so counts
// the try against it. Guessing a key over a LAN is otherwise unlimited and
// silent; ten tries a minute makes it useless without inconveniencing anyone
// typing their own key.
//
// The check and the count are one locked step on purpose. Checking first and
// counting only once the key turned out wrong would let a client fire a burst of
// parallel guesses that all pass the check before any of them is counted. A
// correct key clears the count again (RecordSuccess), so only failures add up.
func (a *Auth) Attempt(client string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.attempts[client]
	if w == nil || now.Sub(w.start) > attemptWin {
		if len(a.attempts) >= maxTrackedIP {
			a.pruneLocked(now)
		}
		a.attempts[client] = &window{count: 1, start: now}
		return true
	}
	if w.count >= maxAttempts {
		return false
	}
	w.count++
	return true
}

// RecordSuccess clears a client's failure count.
func (a *Auth) RecordSuccess(client string) {
	a.mu.Lock()
	delete(a.attempts, client)
	a.mu.Unlock()
}

func (a *Auth) pruneLocked(now time.Time) {
	for k, w := range a.attempts {
		if now.Sub(w.start) > attemptWin {
			delete(a.attempts, k)
		}
	}
}

// Client identifies the caller for rate-limiting purposes. Deliberately the
// direct peer address and not X-Forwarded-For: that header is caller-controlled,
// so trusting it would let one client mint unlimited buckets and defeat the
// limit entirely. Behind a reverse proxy this groups all users together, which
// is the safe direction to be wrong in.
//
// IPv6 peers are grouped by their /64. A single host is normally handed a whole
// /64 and can pick any address in it, so keying on the full address would give
// one attacker 2^64 fresh buckets.
func Client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil {
		return host
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// KeyFromRequest pulls a presented key out of the request: "X-API-Key", or an
// "Authorization: Bearer <key>" header.
func KeyFromRequest(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		return k
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if rest, ok := cutPrefixFold(h, "Bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// Describe is a one-line startup summary, so a typo in the env var name shows up
// as an obviously-open server rather than silent false confidence.
func (a *Auth) Describe() string {
	if !a.Enabled() {
		return "auth: DISABLED (API_KEY not set) — anyone who can reach this port can browse your library"
	}
	msg := fmt.Sprintf("auth: enabled (API_KEY set, sessions last %s)", a.ttl)
	if len(a.key) < MinKeyLen {
		msg += fmt.Sprintf(" — WARNING: API_KEY is only %d characters; use at least %d (e.g. `openssl rand -base64 24`)", len(a.key), MinKeyLen)
	}
	return msg
}

// MinKeyLen is the shortest API_KEY that doesn't draw a startup warning. The rate
// limit slows online guessing, but a short or dictionary key is still the weak
// point, and a stolen session cookie can be brute-forced offline against it.
const MinKeyLen = 16
