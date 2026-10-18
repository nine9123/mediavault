// Command idlegate is a scale-to-zero gatekeeper for Docker containers.
//
// It sits in front of one or more services as a reverse proxy. While a service
// is in use, requests pass straight through. After it has gone untouched for
// IDLE_TIMEOUT it stops the service's containers — freeing all of their memory —
// and the next request that arrives starts them again, waits until the upstream
// answers, and is then served as if nothing happened.
//
// It is generic on purpose: nothing in it knows about mediavault. One idlegate
// (a few MB of RSS) can guard every container on a host; each route is
//
//	LISTEN  UPSTREAM  CONTAINER[,CONTAINER...]  [key=value ...]
//
// e.g.
//
//	ROUTES=":8080 http://mediavault:8080 facerec,mediavault idle=30m
//	        :5432 tcp://postgres:5432    postgres            idle=2h"
//
// Routes are separated by newlines or ';'. Containers start in the order given
// and stop in reverse, so list dependencies first. http:// upstreams are proxied
// per request (and count streams/WebSockets as activity until they close);
// tcp:// upstreams are proxied per connection, for anything that isn't HTTP.
//
// Per-route keys (each defaults to the env var of the same meaning):
//
//	idle=30m           IDLE_TIMEOUT   no traffic for this long → stop
//	start_timeout=2m   START_TIMEOUT  give up on a wake after this long
//	stop_grace=10s     STOP_GRACE     SIGTERM → SIGKILL grace on stop
//	health=/path       (none)         after the port opens, also wait for
//	                                  GET /path to return non-5xx
//
// For a single route, LISTEN / UPSTREAM / CONTAINERS / HEALTH can be used
// instead of ROUTES. DOCKER_HOST selects the Docker API (unix:// socket by
// default, or tcp:// for a docker-socket-proxy).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type route struct {
	listen       string
	upstream     *url.URL
	containers   []string
	idle         time.Duration
	startTimeout time.Duration
	stopGrace    time.Duration
	health       string
}

type defaults struct {
	idle, startTimeout, stopGrace time.Duration
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("idlegate: ")

	routes, dockerHost, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	dk, err := newDocker(dockerHost)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var closers []func()
	for _, r := range routes {
		g := newGate(r, dk)
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := g.sync(sctx)
		cancel()
		if err != nil {
			// Usually a wrong container name or no socket access: say so now,
			// not on the first request hours later.
			log.Fatalf("%s: %v", r.listen, err)
		}
		ln, err := net.Listen("tcp", r.listen)
		if err != nil {
			log.Fatal(err)
		}
		g.mu.Lock()
		state := map[bool]string{true: "awake", false: "asleep"}[g.awake]
		g.mu.Unlock()
		log.Printf("%s → %s %v (idle %s, %s)", r.listen, r.upstream, r.containers, r.idle, state)

		wg.Add(2)
		go func() { defer wg.Done(); g.reapLoop(ctx) }()
		if g.proxy != nil {
			srv := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
			go func() {
				defer wg.Done()
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("%s: %v", r.listen, err)
				}
			}()
			closers = append(closers, func() { _ = srv.Close() })
		} else {
			go func() {
				defer wg.Done()
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go g.serveTCP(c)
				}
			}()
			closers = append(closers, func() { _ = ln.Close() })
		}
	}

	<-ctx.Done()
	// Leave the containers as they are: the gate going away (an update, a
	// reboot) is not a reason to stop or start anything.
	for _, c := range closers {
		c()
	}
	wg.Wait()
}

// loadConfig reads routes from the environment. getenv is injected for tests.
func loadConfig(getenv func(string) string) ([]route, string, error) {
	var d defaults
	var err error
	if d.idle, err = envDur(getenv, "IDLE_TIMEOUT", 30*time.Minute); err != nil {
		return nil, "", err
	}
	if d.startTimeout, err = envDur(getenv, "START_TIMEOUT", 2*time.Minute); err != nil {
		return nil, "", err
	}
	if d.stopGrace, err = envDur(getenv, "STOP_GRACE", 10*time.Second); err != nil {
		return nil, "", err
	}

	spec := getenv("ROUTES")
	if strings.TrimSpace(spec) == "" {
		if getenv("UPSTREAM") == "" {
			return nil, "", errors.New("set ROUTES, or UPSTREAM + CONTAINERS (see README)")
		}
		listen := getenv("LISTEN")
		if listen == "" {
			listen = ":8080"
		}
		spec = listen + " " + getenv("UPSTREAM") + " " + getenv("CONTAINERS")
		if h := getenv("HEALTH"); h != "" {
			spec += " health=" + h
		}
	}
	routes, err := parseRoutes(spec, d)
	if err != nil {
		return nil, "", err
	}
	return routes, getenv("DOCKER_HOST"), nil
}

func envDur(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive duration like 30m", key, v)
	}
	return d, nil
}

func parseRoutes(spec string, d defaults) ([]route, error) {
	var routes []route
	seen := map[string]bool{}
	lines := strings.FieldsFunc(spec, func(r rune) bool { return r == '\n' || r == ';' })
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) < 3 {
			return nil, fmt.Errorf("route %q: want LISTEN UPSTREAM CONTAINERS [key=value...]", line)
		}
		r := route{listen: f[0], idle: d.idle, startTimeout: d.startTimeout, stopGrace: d.stopGrace}
		if !strings.Contains(r.listen, ":") {
			r.listen = ":" + r.listen
		}
		if seen[r.listen] {
			return nil, fmt.Errorf("route %q: %s is already used by another route", line, r.listen)
		}
		seen[r.listen] = true

		u, err := url.Parse(f[1])
		if err != nil || (u.Scheme != "http" && u.Scheme != "tcp") || u.Host == "" || u.Port() == "" {
			return nil, fmt.Errorf("route %q: upstream %q: want http://host:port or tcp://host:port", line, f[1])
		}
		r.upstream = u

		for _, c := range strings.Split(f[2], ",") {
			if c = strings.TrimSpace(c); c != "" {
				r.containers = append(r.containers, c)
			}
		}
		if len(r.containers) == 0 {
			return nil, fmt.Errorf("route %q: no containers", line)
		}

		for _, kv := range f[3:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("route %q: option %q: want key=value", line, kv)
			}
			var dst *time.Duration
			switch k {
			case "idle":
				dst = &r.idle
			case "start_timeout":
				dst = &r.startTimeout
			case "stop_grace":
				dst = &r.stopGrace
			case "health":
				if !strings.HasPrefix(v, "/") {
					return nil, fmt.Errorf("route %q: health=%q: want a path like /health", line, v)
				}
				if u.Scheme != "http" {
					return nil, fmt.Errorf("route %q: health= only applies to http:// upstreams", line)
				}
				r.health = v
				continue
			default:
				return nil, fmt.Errorf("route %q: unknown option %q", line, k)
			}
			dur, err := time.ParseDuration(v)
			if err != nil || dur <= 0 {
				return nil, fmt.Errorf("route %q: %s=%q: want a positive duration", line, k, v)
			}
			*dst = dur
		}
		routes = append(routes, r)
	}
	if len(routes) == 0 {
		return nil, errors.New("no routes configured")
	}
	return routes, nil
}
