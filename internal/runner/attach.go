package runner

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
)

// ErrNoAttach means the daemon is one from before worktrees could be
// attached.
var ErrNoAttach = errors.New("this Leadline cannot attach worktrees; update it")

// Attached is the daemon's answer to an attach, as much of it as is said
// back to whoever asked.
type Attached struct {
	Service     string         `json:"service"`
	Created     bool           `json:"created"`
	Changed     bool           `json:"changed"`
	Connections []AttachedConn `json:"connections"`
	Added       int            `json:"added"`
	Updated     int            `json:"updated"`
	Removed     int            `json:"removed"`
	Skipped     []AttachedSkip `json:"skipped"`
}

// AttachedConn is a connection a worktree owns. It is read from the
// daemon's answer, which has no password in it.
type AttachedConn struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Host     string   `json:"host"`
	Database string   `json:"database"`
	Remote   bool     `json:"remote"`
	Capture  bool     `json:"capture"`
	Env      []string `json:"env"`
	Proxy    *struct {
		Listen string `json:"listen"`
		State  string `json:"state"`
		Error  string `json:"error"`
	} `json:"proxy"`
}

// Through is where an app reaches the connection's database to have its
// queries recorded: the worktree's proxy, when it is listening.
func (c AttachedConn) Through() string {
	if c.Kind != "postgres" || c.Proxy == nil || c.Proxy.State != "listening" {
		return ""
	}
	return c.Proxy.Listen
}

// AttachedSkip is a URL the daemon was given and did not save.
type AttachedSkip struct {
	Env    []string `json:"env"`
	Reason string   `json:"reason"`
}

// Detached is the daemon's answer to a detach.
type Detached struct {
	Service     string `json:"service"`
	Found       bool   `json:"found"`
	Connections int    `json:"connections"`
	Proxies     int    `json:"proxies"`
	KeptData    bool   `json:"kept_data"`
	Deleted     struct {
		Spans        int64 `json:"spans"`
		Logs         int64 `json:"logs"`
		MetricPoints int64 `json:"metric_points"`
	} `json:"deleted"`
}

// Attach registers a worktree with the daemon as a source of its own, with
// the connections found for it. The URLs go from here to the daemon and
// nowhere else. capture false saves them for their stats only. partial says
// the URLs may not be all of the worktree's (nowhere was found to read them
// from), so the connections it already owns are left alone.
func (d *Daemon) Attach(ctx context.Context, w Worktree, conns []ConnURL, capture, partial bool) (Attached, error) {
	type urlIn struct {
		Env []string `json:"env"`
		URL string   `json:"url"`
	}
	body := struct {
		Service     string  `json:"service"`
		Via         string  `json:"via"`
		Project     string  `json:"project"`
		Worktree    string  `json:"worktree"`
		Branch      string  `json:"branch"`
		Box         string  `json:"box"`
		Path        string  `json:"path"`
		Connections []urlIn `json:"connections"`
		Capture     *bool   `json:"capture,omitempty"`
		Partial     bool    `json:"partial,omitempty"`
	}{Service: w.Service(), Via: w.Via, Project: w.Project, Worktree: w.Name, Branch: w.Branch, Box: w.Box, Path: w.Path,
		Connections: []urlIn{}, Partial: partial}
	if !capture {
		body.Capture = &capture
	}
	for _, c := range conns {
		keys := append([]string(nil), c.Keys...)
		sort.Strings(keys)
		body.Connections = append(body.Connections, urlIn{Env: keys, URL: c.raw})
	}
	var out Attached
	status, err := d.call(ctx, http.MethodPost, "/api/attach", body, &out)
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return out, ErrNoAttach
	}
	return out, err
}

// Detach has the daemon forget a worktree: its connections, its proxies
// and, unless keepData, what was recorded under its name.
func (d *Daemon) Detach(ctx context.Context, service string, keepData bool) (Detached, error) {
	var out Detached
	// Deleting a worktree's data from a large store can take longer than
	// the two seconds every other call gets; the caller's context decides.
	long := &Daemon{URL: d.URL, HTTP: &http.Client{Transport: d.HTTP.Transport}}
	status, err := long.call(ctx, http.MethodPost, "/api/detach",
		map[string]any{"service": service, "keep_data": keepData}, &out)
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return out, ErrNoAttach
	}
	return out, err
}

// AttachedTo asks which connections the daemon holds for an attached
// worktree. ok is false when the worktree is not attached, or the daemon
// cannot say.
func (d *Daemon) AttachedTo(ctx context.Context, service string) (conns []AttachedConn, ok bool) {
	var out struct {
		Connections []AttachedConn `json:"connections"`
	}
	if _, err := d.call(ctx, http.MethodGet, "/api/attach?service="+url.QueryEscape(service), nil, &out); err != nil {
		return nil, false
	}
	return out.Connections, true
}
