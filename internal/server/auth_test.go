package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mediavault/internal/config"
)

const testKey = "correct-horse-battery-staple"

// newTestServer builds a Server with no store: these tests exercise the auth
// middleware, which must decide before any handler touches the database.
func newTestServer(key string) *Server {
	return New(config.Config{APIKey: key, AuthTTL: time.Hour}, nil, nil, nil, nil)
}

// sentinel stands in for a real route, so "did the request get through?" is
// unambiguous and doesn't depend on a handler's own behaviour.
func sentinel() (http.Handler, *bool) {
	reached := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), &reached
}

func TestUnauthenticatedApiIsBlocked(t *testing.T) {
	s := newTestServer(testKey)
	h, reached := sentinel()
	guarded := s.requireAuth(h)

	for _, path := range []string{
		"/api/items", "/api/search", "/api/stats", "/api/thumb/a/b.jpg",
		"/api/preview/a/b.mp4", "/api/raw/a/b.mp4", "/api/item", "/api/random",
	} {
		*reached = false
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", path, rec.Code)
		}
		if *reached {
			t.Errorf("%s: request reached the handler without a session", path)
		}
	}
}

// The browser fetches media through <img>/<video>, which cannot send headers.
// If these were not cookie-authenticated the whole grid would 401.
func TestMediaPathsAcceptTheSessionCookie(t *testing.T) {
	s := newTestServer(testKey)
	h, reached := sentinel()
	guarded := s.requireAuth(h)

	token, expires := s.auth.Issue(time.Now())
	_ = expires
	for _, path := range []string{"/api/thumb/x.jpg", "/api/preview/x.mp4", "/api/raw/x.mp4"} {
		*reached = false
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK || !*reached {
			t.Errorf("%s: cookie-authenticated request blocked (status %d)", path, rec.Code)
		}
	}
}

func TestHeaderAuthForScripts(t *testing.T) {
	s := newTestServer(testKey)
	h, reached := sentinel()
	guarded := s.requireAuth(h)

	for _, tc := range []struct {
		name, header, value string
		want                int
	}{
		{"x-api-key", "X-API-Key", testKey, http.StatusOK},
		{"bearer", "Authorization", "Bearer " + testKey, http.StatusOK},
		{"wrong key", "X-API-Key", "nope", http.StatusUnauthorized},
	} {
		*reached = false
		r := httptest.NewRequest("GET", "/api/items", nil)
		r.Header.Set(tc.header, tc.value)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// The shell must load unauthenticated or there is nowhere to type the key.
func TestPublicPaths(t *testing.T) {
	s := newTestServer(testKey)
	h, reached := sentinel()
	guarded := s.requireAuth(h)

	for _, path := range []string{"/", "/api/auth"} {
		*reached = false
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if !*reached {
			t.Errorf("%s should be public, got status %d", path, rec.Code)
		}
	}
	// ...but nothing else is public, including near-misses.
	for _, path := range []string{"/api/authx", "/api/auth/extra", "/index.html"} {
		*reached = false
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", path, rec.Code)
		}
	}
}

func TestNoKeyConfiguredLeavesEverythingOpen(t *testing.T) {
	s := newTestServer("")
	h, reached := sentinel()
	guarded := s.requireAuth(h)
	*reached = false
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest("GET", "/api/items", nil))
	if !*reached {
		t.Error("with no API_KEY set, requests should pass through unchanged")
	}
}

// A browser dialog on top of the app's own login screen would be confusing, so
// the 401 must not advertise a challenge scheme.
func TestNoWWWAuthenticateChallenge(t *testing.T) {
	s := newTestServer(testKey)
	h, _ := sentinel()
	rec := httptest.NewRecorder()
	s.requireAuth(h).ServeHTTP(rec, httptest.NewRequest("GET", "/api/items", nil))
	if v := rec.Header().Get("WWW-Authenticate"); v != "" {
		t.Errorf("WWW-Authenticate should be absent, got %q", v)
	}
}

func TestLoginFlow(t *testing.T) {
	s := newTestServer(testKey)
	mux := s.Routes()

	// Wrong key.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, jsonReq("POST", "/api/auth", `{"key":"wrong"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a failed login must not set a cookie")
	}

	// Right key.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, jsonReq("POST", "/api/auth", `{"key":"`+testKey+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("correct key: status %d, want 200", rec.Code)
	}
	var c *http.Cookie
	for _, got := range rec.Result().Cookies() {
		if got.Name == sessionCookie {
			c = got
		}
	}
	if c == nil {
		t.Fatal("no session cookie issued")
	}
	if !c.HttpOnly {
		t.Error("session cookie should be HttpOnly")
	}
	if c.Secure {
		t.Error("Secure must not be set for a plain-HTTP request, or the browser drops the cookie")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("cookie path = %q, want /", c.Path)
	}
	if strings.Contains(c.Value, testKey) {
		t.Error("the cookie must not contain the API key itself")
	}

	// The issued cookie really opens the door.
	h, reached := sentinel()
	r := httptest.NewRequest("GET", "/api/items", nil)
	r.AddCookie(c)
	rec2 := httptest.NewRecorder()
	s.requireAuth(h).ServeHTTP(rec2, r)
	if !*reached {
		t.Error("cookie from login did not authenticate a later request")
	}
}

func TestSecureFlagOverTLS(t *testing.T) {
	s := newTestServer(testKey)
	r := jsonReq("POST", "/api/auth", `{"key":"`+testKey+`"}`)
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, r)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && !c.Secure {
			t.Error("Secure should be set when the request arrived over HTTPS")
		}
	}
}

func TestAuthStatus(t *testing.T) {
	s := newTestServer(testKey)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth", nil))
	var got struct {
		Enabled bool `json:"enabled"`
		Authed  bool `json:"authed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Authed {
		t.Errorf("fresh visitor: enabled=%v authed=%v, want true/false", got.Enabled, got.Authed)
	}

	// With no key configured the UI should skip the prompt entirely.
	rec = httptest.NewRecorder()
	newTestServer("").Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth", nil))
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled || !got.Authed {
		t.Errorf("no key: enabled=%v authed=%v, want false/true", got.Enabled, got.Authed)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	s := newTestServer(testKey)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/auth", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if c.MaxAge >= 0 {
				t.Errorf("logout should expire the cookie, MaxAge=%d", c.MaxAge)
			}
			if c.Value != "" {
				t.Errorf("logout should blank the cookie, got %q", c.Value)
			}
			return
		}
	}
	t.Error("logout did not send a clearing cookie")
}

func TestLoginRateLimited(t *testing.T) {
	s := newTestServer(testKey)
	mux := s.Routes()
	var got429 bool
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonReq("POST", "/api/auth", `{"key":"guess"}`))
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("repeated wrong keys were never rate limited")
	}
	// A correct key is still refused while the client is throttled, so the limit
	// can't be sidestepped by interleaving a real attempt.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, jsonReq("POST", "/api/auth", `{"key":"`+testKey+`"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("throttled client: status %d, want 429", rec.Code)
	}
}

func jsonReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.URL, _ = url.Parse(path)
	return r
}

// idlegate polls /healthz with no key after a wake. A 401 there would read as
// "ready" to it (anything under 500 does), which is why /api/stats was no check.
func TestHealthzIsPublicAndOK(t *testing.T) {
	h := newTestServer(testKey).Routes()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz without a key: status %d, want 200", rec.Code)
	}
}

// A key in a header is a guess too. Without a limit on it, any /api/ route would
// be an unthrottled way around the login form's limit.
func TestHeaderKeyGuessesAreRateLimited(t *testing.T) {
	s := newTestServer(testKey)
	h, reached := sentinel()
	guarded := s.requireAuth(h)
	var got429 bool
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("GET", "/api/items", nil)
		r.Header.Set("X-API-Key", "guess")
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("repeated wrong header keys were never rate limited")
	}
	// Throttled means the key isn't even checked, right or wrong.
	*reached = false
	r := httptest.NewRequest("GET", "/api/items", nil)
	r.Header.Set("X-API-Key", testKey)
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests || *reached {
		t.Errorf("throttled client with the right key: status %d reached=%v, want 429 and blocked", rec.Code, *reached)
	}
	// ...and the login form shares the same budget.
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, jsonReq("POST", "/api/auth", `{"key":"`+testKey+`"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("login after header guesses: status %d, want 429", rec.Code)
	}
}

// A script with the right key must not run itself into the limit.
func TestCorrectHeaderKeyNeverThrottles(t *testing.T) {
	s := newTestServer(testKey)
	h, _ := sentinel()
	guarded := s.requireAuth(h)
	for i := 0; i < 50; i++ {
		r := httptest.NewRequest("GET", "/api/items", nil)
		r.Header.Set("X-API-Key", testKey)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d with the correct key: status %d", i+1, rec.Code)
		}
	}
}

func TestCrossSiteWritesRefused(t *testing.T) {
	h, reached := sentinel()
	guarded := rejectCrossSite(h)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    bool // refused?
	}{
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, true},
		// Another app on the same NAS, different port: same-site, still not us.
		{"same-site other port", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://example.com:9000"}, true},
		{"same-origin UI", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://example.com"}, false},
		{"old browser, foreign Origin", map[string]string{"Origin": "http://evil.example"}, true},
		{"old browser, null Origin", map[string]string{"Origin": "null"}, true},
		{"old browser, own Origin", map[string]string{"Origin": "http://example.com"}, false},
		{"curl, no browser headers", nil, false},
	} {
		*reached = false
		r := httptest.NewRequest("POST", "/api/scan", nil) // Host: example.com
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)
		if refused := rec.Code == http.StatusForbidden && !*reached; refused != tc.want {
			t.Errorf("%s: status %d, refused=%v want %v", tc.name, rec.Code, refused, tc.want)
		}
	}
}

// The guard has to be on the real handler chain, and in front of auth: it must
// hold with no API_KEY too, where there is no cookie for SameSite to withhold.
func TestCrossSiteGuardIsWired(t *testing.T) {
	for _, key := range []string{testKey, ""} {
		r := httptest.NewRequest("DELETE", "/api/auth", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		newTestServer(key).Routes().ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("key=%q: cross-site DELETE status %d, want 403", key, rec.Code)
		}
	}
}

// Reads stay open to cross-site navigation (a bookmarked /api/raw link, an
// <img> tag): nothing changes on a GET, and blocking them would break those.
func TestCrossSiteReadsPass(t *testing.T) {
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	newTestServer(testKey).Routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("cross-site GET: status %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	for _, path := range []string{"/", "/api/items", "/api/auth", "/healthz"} {
		rec := httptest.NewRecorder()
		newTestServer(testKey).Routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		for k, want := range map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Content-Security-Policy": "frame-ancestors 'none'",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := rec.Header().Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", path, k, got, want)
			}
		}
	}
}

func TestLoginBodyIsCapped(t *testing.T) {
	rec := httptest.NewRecorder()
	body := `{"key":"` + strings.Repeat("a", 64<<10) + `"}`
	newTestServer(testKey).Routes().ServeHTTP(rec, jsonReq("POST", "/api/auth", body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("64 KiB login body: status %d, want 400", rec.Code)
	}
}
