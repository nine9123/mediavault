package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDocker is a Docker API with one "container" per name. Starting one serves
// HTTP (or echoes TCP) on its reserved address; stopping it closes the listener,
// so the gate sees exactly what it would see with a real container.
type fakeDocker struct {
	t      *testing.T
	mu     sync.Mutex
	addr   map[string]string // container → listen address
	ln     map[string]net.Listener
	tcp    bool
	starts atomic.Int32
	stops  atomic.Int32
	order  []string
}

func newFakeDocker(t *testing.T, names ...string) (*fakeDocker, *docker) {
	f := &fakeDocker{t: t, addr: map[string]string{}, ln: map[string]net.Listener{}}
	for _, n := range names {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		f.addr[n] = l.Addr().String()
		l.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		srv.Close()
		f.mu.Lock()
		for _, l := range f.ln {
			l.Close()
		}
		f.mu.Unlock()
	})
	dk, err := newDocker("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return f, dk
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // containers/NAME/op
	if len(parts) != 3 || parts[0] != "containers" {
		http.NotFound(w, r)
		return
	}
	name, op := parts[1], parts[2]
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.addr[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: `+name+`"}`)
		return
	}
	_, running := f.ln[name]
	switch op {
	case "json":
		io.WriteString(w, `{"State":{"Running":`+map[bool]string{true: "true", false: "false"}[running]+`}}`)
	case "start":
		f.order = append(f.order, "start "+name)
		if running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			f.t.Errorf("fake start %s: %v", name, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.ln[name] = l
		f.starts.Add(1)
		if f.tcp {
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					go func() { io.Copy(c, c); c.Close() }()
				}
			}()
		} else {
			// An http.Server, not http.Serve: stopping has to drop open
			// keep-alive connections too, as a real container stop would.
			hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/slow" {
					time.Sleep(300 * time.Millisecond)
				}
				io.WriteString(w, "hello from "+name)
			})}
			f.ln[name] = closerListener{l, hs}
			go hs.Serve(l)
		}
		w.WriteHeader(http.StatusNoContent)
	case "stop":
		f.order = append(f.order, "stop "+name)
		if !running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.ln[name].Close()
		delete(f.ln, name)
		f.stops.Add(1)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// closerListener makes stopping an HTTP "container" close its server (and so
// its connections), not just its listener.
type closerListener struct {
	net.Listener
	hs *http.Server
}

func (c closerListener) Close() error { return c.hs.Close() }

func (f *fakeDocker) running(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.ln[name]
	return ok
}

func testRoute(t *testing.T, scheme, upstreamAddr string, containers ...string) route {
	u, _ := url.Parse(scheme + "://" + upstreamAddr)
	return route{
		listen: "test", upstream: u, containers: containers,
		idle: time.Hour, startTimeout: 5 * time.Second, stopGrace: time.Second,
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWakeServeAndSleep(t *testing.T) {
	f, dk := newFakeDocker(t, "db", "app")
	g := newGate(testRoute(t, "http", f.addr["app"], "db", "app"), dk)
	clock := time.Now()
	g.now = func() time.Time { return clock }
	if err := g.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	defer srv.Close()

	// Asleep → first request boots both containers, in order, then is served.
	if code, body := get(t, srv.URL); code != 200 || body != "hello from app" {
		t.Fatalf("got %d %q", code, body)
	}
	if !f.running("db") || !f.running("app") {
		t.Fatal("containers not started")
	}

	// Not idle long enough: nothing happens.
	clock = clock.Add(59 * time.Minute)
	if g.reap() != nil {
		t.Fatal("reaped before the idle timeout")
	}

	// Idle past the timeout: stopped, dependents first.
	clock = clock.Add(2 * time.Minute)
	done := g.reap()
	if done == nil {
		t.Fatal("did not reap after the idle timeout")
	}
	<-done
	if f.running("db") || f.running("app") {
		t.Fatal("containers still running after sleep")
	}
	want := "start db,start app,stop app,stop db"
	if got := strings.Join(f.order, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}

	// And the next request wakes it again.
	if code, _ := get(t, srv.URL); code != 200 {
		t.Fatalf("after re-wake got %d", code)
	}
	if n := f.starts.Load(); n != 4 {
		t.Fatalf("starts = %d, want 4", n)
	}
}

func TestConcurrentRequestsShareOneWake(t *testing.T) {
	f, dk := newFakeDocker(t, "app")
	g := newGate(testRoute(t, "http", f.addr["app"], "app"), dk)
	if err := g.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := get(t, srv.URL); code != 200 {
				t.Errorf("got %d", code)
			}
		}()
	}
	wg.Wait()
	if n := f.starts.Load(); n != 1 {
		t.Fatalf("starts = %d, want 1", n)
	}
}

func TestInflightRequestBlocksSleep(t *testing.T) {
	f, dk := newFakeDocker(t, "app")
	g := newGate(testRoute(t, "http", f.addr["app"], "app"), dk)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	g.now = func() time.Time { return time.Unix(0, clock.Load()) }
	if err := g.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	defer srv.Close()
	get(t, srv.URL) // wake

	// A long request is in flight while the clock jumps past the timeout.
	result := make(chan int)
	go func() { code, _ := get(t, srv.URL+"/slow"); result <- code }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		n := g.inflight
		g.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never went in flight")
		}
		time.Sleep(5 * time.Millisecond)
	}
	clock.Add(int64(2 * time.Hour))
	if g.reap() != nil {
		t.Fatal("reaped with a request in flight")
	}
	if code := <-result; code != 200 {
		t.Fatalf("slow request got %d", code)
	}
	// Finishing the request counts as activity: still not idle.
	if g.reap() != nil {
		t.Fatal("reaped right after a request finished")
	}
}

func TestStartupSyncKeepsRunningContainersAwake(t *testing.T) {
	f, dk := newFakeDocker(t, "app")
	g := newGate(testRoute(t, "http", f.addr["app"], "app"), dk)
	// Already running before the gate starts.
	if err := dk.start(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	if err := g.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.awake {
		t.Fatal("running container not seen as awake")
	}
	if g.reap() != nil {
		t.Fatal("reaped immediately on startup")
	}
}

func TestExternallyStoppedContainerIsRestarted(t *testing.T) {
	f, dk := newFakeDocker(t, "app")
	g := newGate(testRoute(t, "http", f.addr["app"], "app"), dk)
	if err := g.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	defer srv.Close()
	get(t, srv.URL)

	dk.stop(context.Background(), "app", 0) // behind the gate's back
	if code, _ := get(t, srv.URL); code != http.StatusBadGateway {
		t.Fatalf("first request after external stop got %d, want 502", code)
	}
	if code, _ := get(t, srv.URL); code != 200 {
		t.Fatalf("second request got %d, want a re-wake and 200", code)
	}
}

func TestWakeFailureIsReported(t *testing.T) {
	f, dk := newFakeDocker(t, "app")
	g := newGate(testRoute(t, "http", f.addr["app"], "nope"), dk)
	srv := httptest.NewServer(g)
	defer srv.Close()
	code, body := get(t, srv.URL)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "No such container: nope") {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestTCPRoute(t *testing.T) {
	f, dk := newFakeDocker(t, "echo")
	f.tcp = true
	g := newGate(testRoute(t, "tcp", f.addr["echo"], "echo"), dk)
	if err := g.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go g.serveTCP(c)
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "ping")
	c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	c.Close()
	if string(b) != "ping" {
		t.Fatalf("echo got %q", b)
	}
	if !f.running("echo") {
		t.Fatal("tcp route did not start its container")
	}
}

func TestParseRoutes(t *testing.T) {
	d := defaults{idle: 30 * time.Minute, startTimeout: time.Minute, stopGrace: 10 * time.Second}
	rs, err := parseRoutes(`
		# comment
		:8080 http://mediavault:8080 facerec,mediavault idle=1h health=/api/stats
		5432  tcp://pg:5432 pg ; :9000 http://x:9000 x stop_grace=30s`, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("got %d routes", len(rs))
	}
	if r := rs[0]; r.idle != time.Hour || r.health != "/api/stats" || strings.Join(r.containers, ",") != "facerec,mediavault" {
		t.Fatalf("route 0 = %+v", r)
	}
	if r := rs[1]; r.listen != ":5432" || r.upstream.Scheme != "tcp" || r.idle != d.idle {
		t.Fatalf("route 1 = %+v", r)
	}
	if r := rs[2]; r.stopGrace != 30*time.Second {
		t.Fatalf("route 2 = %+v", r)
	}

	for _, bad := range []string{
		":8080 http://app app,",          // no port
		":8080 ftp://app:21 app",         // bad scheme
		":8080 http://app:80",            // no containers
		":8080 http://app:80 app idle=x", // bad duration
		":8080 http://app:80 app foo=1",  // unknown key
		":8080 tcp://app:80 app health=/h",
		":8080 http://a:1 a; :8080 http://b:1 b", // duplicate listen
		"",
	} {
		if _, err := parseRoutes(bad, d); err == nil {
			t.Errorf("parseRoutes(%q) accepted", bad)
		}
	}
}

func TestLoadConfigSingleRoute(t *testing.T) {
	env := map[string]string{"UPSTREAM": "http://app:3000", "CONTAINERS": "app", "IDLE_TIMEOUT": "5m"}
	rs, _, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].listen != ":8080" || rs[0].idle != 5*time.Minute {
		t.Fatalf("got %+v", rs)
	}
}
