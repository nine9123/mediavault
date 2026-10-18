package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// docker is the smallest Docker Engine API client that does the job: inspect,
// start, stop. Three endpoints don't justify the official SDK (and its few
// hundred transitive packages); the API is plain HTTP over a unix socket.
//
// Paths are unversioned, which the daemon serves as its own current version.
// The three calls used here have been stable since API 1.12.
type docker struct {
	base string // e.g. "http://docker" for a socket, "http://host:2375" for tcp
	hc   *http.Client
}

// newDocker accepts the same DOCKER_HOST forms the CLI does that make sense in a
// container: unix:///path/to/docker.sock (default) or tcp://host:port — the
// latter is how you'd point at a docker-socket-proxy instead of the raw socket.
func newDocker(host string) (*docker, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST %q: %w", host, err)
	}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
		return &docker{base: "http://docker", hc: &http.Client{Transport: tr}}, nil
	case "tcp", "http":
		return &docker{base: "http://" + u.Host, hc: &http.Client{}}, nil
	default:
		return nil, fmt.Errorf("DOCKER_HOST %q: want unix:// or tcp://", host)
	}
}

func (d *docker) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, nil)
	if err != nil {
		return nil, err
	}
	return d.hc.Do(req)
}

// apiError turns a non-2xx response into an error carrying the daemon's message
// ({"message": "..."}), which is usually the useful part ("No such container").
func apiError(op, name string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var m struct{ Message string }
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return fmt.Errorf("docker %s %s: %s", op, name, m.Message)
	}
	return fmt.Errorf("docker %s %s: %s %s", op, name, resp.Status, strings.TrimSpace(string(body)))
}

// running reports whether the named container is currently running.
func (d *docker) running(ctx context.Context, name string) (bool, error) {
	resp, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, apiError("inspect", name, resp)
	}
	var c struct {
		State struct{ Running bool }
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return false, fmt.Errorf("docker inspect %s: %w", name, err)
	}
	return c.State.Running, nil
}

// start starts a container. Already running (304) is success.
func (d *docker) start(ctx context.Context, name string) error {
	resp, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		return apiError("start", name, resp)
	}
	return nil
}

// stop stops a container, giving it grace to exit on SIGTERM before SIGKILL.
// Already stopped (304) is success.
func (d *docker) stop(ctx context.Context, name string, grace time.Duration) error {
	path := fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(name), int(grace.Seconds()))
	resp, err := d.do(ctx, http.MethodPost, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		return apiError("stop", name, resp)
	}
	return nil
}
