package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// gate owns one route: a listener, the upstream it forwards to, and the
// containers that have to be running for that upstream to answer.
//
// State is three fields under one mutex:
//
//	awake    the containers are (as far as we know) up and answering
//	busy     non-nil while a start or a stop is in progress; closed when it ends
//	inflight requests/connections currently being served (or waiting on a wake)
//
// Only one transition runs at a time, and every request that arrives during one
// waits on busy rather than starting its own. A request counts as in flight from
// the moment it arrives — before the wake — so the idle reaper can never stop
// the containers out from under a request that is waiting for them.
type gate struct {
	route
	dk *docker

	mu       sync.Mutex
	awake    bool
	busy     chan struct{}
	wakeErr  error // result of the last wake; read by waiters once busy closes
	inflight int
	last     time.Time // last activity: a request arriving or finishing

	proxy *httputil.ReverseProxy // nil for tcp:// routes
	now   func() time.Time       // swapped in tests
}

func newGate(r route, dk *docker) *gate {
	g := &gate{route: r, dk: dk, now: time.Now}
	if r.upstream.Scheme != "tcp" {
		g.proxy = httputil.NewSingleHostReverseProxy(r.upstream)
		g.proxy.ErrorHandler = g.proxyError
	}
	return g
}

// sync aligns the gate with reality at startup. If everything is already
// running, treat it as freshly used: it gets one full idle period before it is
// put to sleep, so restarting the gate never yanks a container you are using.
func (g *gate) sync(ctx context.Context) error {
	all := true
	for _, c := range g.containers {
		up, err := g.dk.running(ctx, c)
		if err != nil {
			return err
		}
		all = all && up
	}
	g.mu.Lock()
	g.awake, g.last = all, g.now()
	g.mu.Unlock()
	return nil
}

func (g *gate) begin() {
	g.mu.Lock()
	g.inflight++
	g.last = g.now()
	g.mu.Unlock()
}

func (g *gate) end() {
	g.mu.Lock()
	g.inflight--
	g.last = g.now()
	g.mu.Unlock()
}

// ensureAwake returns once the upstream is up, starting it if needed. The
// caller must already hold an inflight slot (begin).
func (g *gate) ensureAwake(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.awake && g.busy == nil {
			g.mu.Unlock()
			return nil
		}
		if g.busy == nil {
			g.busy = make(chan struct{})
			go g.wake(g.busy)
		}
		busy := g.busy
		g.mu.Unlock()

		select {
		case <-busy:
		case <-ctx.Done():
			return ctx.Err()
		}

		g.mu.Lock()
		awake, err := g.awake, g.wakeErr
		g.mu.Unlock()
		if !awake && err != nil {
			return err
		}
		// Either awake now, or what we waited on was a stop: loop and wake.
	}
}

// wake starts the containers in order and waits for the upstream to accept
// connections. It deliberately does not use any request's context: a client
// giving up mid-boot shouldn't abort a boot that the next click will want.
func (g *gate) wake(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), g.startTimeout)
	defer cancel()
	t0 := g.now()
	log.Printf("%s: waking %v", g.listen, g.containers)

	err := func() error {
		for _, c := range g.containers {
			if err := g.dk.start(ctx, c); err != nil {
				return err
			}
		}
		return waitReady(ctx, g.upstream, g.health)
	}()

	g.mu.Lock()
	g.wakeErr = err
	g.awake = err == nil
	g.last = g.now()
	g.busy = nil
	g.mu.Unlock()
	close(done)

	if err != nil {
		log.Printf("%s: wake failed: %v", g.listen, err)
	} else {
		log.Printf("%s: awake in %s", g.listen, g.now().Sub(t0).Round(time.Millisecond))
	}
}

// sleep stops the containers in reverse order (dependents before what they
// depend on). On failure it leaves the gate marked awake so the next reap tries
// again; a stop error must never turn into failed requests.
func (g *gate) sleep(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), g.stopGrace+30*time.Second)
	defer cancel()
	log.Printf("%s: idle for %s, stopping %v", g.listen, g.idle, g.containers)

	var err error
	for i := len(g.containers) - 1; i >= 0; i-- {
		if e := g.dk.stop(ctx, g.containers[i], g.stopGrace); e != nil {
			err = errors.Join(err, e)
		}
	}

	g.mu.Lock()
	g.awake = err != nil
	g.last = g.now()
	g.busy = nil
	g.mu.Unlock()
	close(done)

	if err != nil {
		log.Printf("%s: stop failed (will retry): %v", g.listen, err)
	} else {
		log.Printf("%s: asleep", g.listen)
	}
}

// reap puts the route to sleep if it has been idle long enough. It returns the
// transition's done channel (nil if nothing started) so tests can wait on it.
func (g *gate) reap() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.awake || g.busy != nil || g.inflight > 0 || g.now().Sub(g.last) < g.idle {
		return nil
	}
	g.busy = make(chan struct{})
	go g.sleep(g.busy)
	return g.busy
}

// reapLoop checks for idleness until ctx ends. The tick only bounds how late a
// stop can be, so it is coarse: a tenth of the timeout, capped at 30s.
func (g *gate) reapLoop(ctx context.Context) {
	tick := min(g.idle/10, 30*time.Second)
	tick = max(tick, 50*time.Millisecond)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.reap()
		}
	}
}

// ServeHTTP is the http:// route handler.
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.begin()
	defer g.end()
	if err := g.ensureAwake(r.Context()); err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "idlegate: upstream failed to start: "+err.Error(), http.StatusServiceUnavailable)
		}
		return
	}
	// The handler doesn't return until the response is done, so long video
	// streams and upgraded (WebSocket) connections keep the route awake.
	g.proxy.ServeHTTP(w, r)
}

// proxyError handles an upstream that refuses or drops the connection. If the
// containers were stopped behind our back (docker stop, a crash), forget that
// they were awake so the next request starts them again.
func (g *gate) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		g.mu.Lock()
		if g.busy == nil {
			g.awake = false
		}
		g.mu.Unlock()
	}
	if r.Context().Err() == nil {
		log.Printf("%s: proxy %s %s: %v", g.listen, r.Method, r.URL.Path, err)
	}
	w.WriteHeader(http.StatusBadGateway)
}

// serveTCP is the tcp:// route: one inflight slot per client connection, bytes
// copied both ways until either side closes.
func (g *gate) serveTCP(c net.Conn) {
	defer c.Close()
	g.begin()
	defer g.end()
	ctx, cancel := context.WithTimeout(context.Background(), g.startTimeout)
	err := g.ensureAwake(ctx)
	cancel()
	if err != nil {
		return
	}
	up, err := net.DialTimeout("tcp", g.upstream.Host, 10*time.Second)
	if err != nil {
		g.mu.Lock()
		if g.busy == nil {
			g.awake = false
		}
		g.mu.Unlock()
		log.Printf("%s: dial %s: %v", g.listen, g.upstream.Host, err)
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-done
	<-done
}

// waitReady polls until the upstream accepts a TCP connection — and, if a
// health path is set, until GETting it returns a non-5xx status. Listening is
// not always the same as ready; the health path covers apps that bind early.
func waitReady(ctx context.Context, u *url.URL, health string) error {
	hc := &http.Client{Timeout: 5 * time.Second}
	var last error
	for {
		var d net.Dialer
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := d.DialContext(dctx, "tcp", u.Host)
		cancel()
		if err == nil {
			c.Close()
			if health == "" {
				return nil
			}
			err = checkHealth(ctx, hc, u, health)
			if err == nil {
				return nil
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready: %w (last: %v)", u.Host, ctx.Err(), last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func checkHealth(ctx context.Context, hc *http.Client, u *url.URL, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.Host+path, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("health %s: %s", path, resp.Status)
	}
	return nil
}
