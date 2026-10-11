package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultURL is where the daemon listens unless told otherwise.
const DefaultURL = "http://127.0.0.1:4320"

// ErrNoProxies means the daemon is one from before it could start capture
// proxies on request.
var ErrNoProxies = errors.New("this Leadline cannot start a database proxy; update it")

// Daemon is the little of the daemon's API that a run needs. Every call is
// given a short time: the daemon is on this machine, and a run never waits
// on it for long.
type Daemon struct {
	URL  string
	HTTP *http.Client
}

// NewDaemon returns a client for the daemon at url.
func NewDaemon(url string) *Daemon {
	return &Daemon{
		URL: strings.TrimRight(strings.TrimSpace(url), "/"),
		HTTP: &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				// The daemon is local; a proxy from the environment has no
				// business with it.
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext,
				MaxIdleConns:        2,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
				MaxIdleConnsPerHost: 2,
			},
		},
	}
}

// Healthy reports whether a Leadline daemon answers at the URL, and not
// merely something that listens there.
func (d *Daemon) Healthy(ctx context.Context) bool {
	var health struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	if _, err := d.call(ctx, http.MethodGet, "/api/health", nil, &health); err != nil {
		return false
	}
	return health.OK && health.Name == "leadline"
}

// Proxy is a capture proxy the daemon runs for one database.
type Proxy struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
}

// AddProxy asks for a Postgres proxy in front of upstream. ErrNoProxies
// means the daemon has no such endpoint.
func (d *Daemon) AddProxy(ctx context.Context, upstream, label string) (Proxy, error) {
	var p Proxy
	status, err := d.call(ctx, http.MethodPost, "/api/proxies",
		map[string]string{"kind": "postgres", "upstream": upstream, "label": label}, &p)
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return p, ErrNoProxies
	}
	if err != nil {
		return p, err
	}
	if _, _, err := net.SplitHostPort(p.Listen); err != nil || p.ID == "" {
		return p, fmt.Errorf("the daemon's answer names no proxy address")
	}
	return p, nil
}

// RemoveProxy stops a proxy AddProxy started.
func (d *Daemon) RemoveProxy(ctx context.Context, id string) error {
	_, err := d.call(ctx, http.MethodDelete, "/api/proxies/"+id, nil, nil)
	return err
}

// RunInfo is what the daemon is told about a run, and what GET /api/runs
// gives back (internal/api/runs.go).
type RunInfo struct {
	ID      string   `json:"id"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd"`
	Service string   `json:"service"`
	// The command's process, and the `leadline run` that started it.
	PID       int `json:"pid"`
	RunnerPID int `json:"runner_pid"`
	// How the command was set up: "node" when the Node provider was loaded.
	Provider string     `json:"provider,omitempty"`
	Proxies  []RunProxy `json:"proxies,omitempty"`
	Terminal bool       `json:"terminal"`
}

// RunProxy is a database a run reaches through a capture proxy.
type RunProxy struct {
	Kind     string `json:"kind"`
	Env      string `json:"env"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	// attached: the proxy is the worktree's own, there before the run and
	// after it, and not one opened for the run.
	attached bool
}

// StartRun tells the daemon a command has started.
func (d *Daemon) StartRun(ctx context.Context, run RunInfo) error {
	_, err := d.call(ctx, http.MethodPost, "/api/runs", run, nil)
	return err
}

// EndRun tells the daemon how a command ended. signal is its name when a
// signal ended it ("SIGTERM"), else empty.
func (d *Daemon) EndRun(ctx context.Context, id string, exitCode int, signal string) error {
	_, err := d.call(ctx, http.MethodPost, "/api/runs/"+id+"/end",
		map[string]any{"exit_code": exitCode, "signal": signal}, nil)
	return err
}

func (d *Daemon) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.URL+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		// The daemon's own errors are a sentence for a person; say that.
		var problem struct {
			Error struct {
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		}
		if json.Unmarshal(text, &problem) == nil && problem.Error.Message != "" {
			return resp.StatusCode, errors.New(strings.TrimSpace(problem.Error.Message + " " + problem.Error.Hint))
		}
		if len(text) > 300 {
			text = text[:300]
		}
		// The query string stays out of it: it may be long, and it is the
		// caller's own.
		shown, _, _ := strings.Cut(path, "?")
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, shown, resp.Status, bytes.TrimSpace(text))
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}
