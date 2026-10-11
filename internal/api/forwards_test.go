package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean-brydon/leadline/internal/discover"
	"github.com/sean-brydon/leadline/internal/discover/fakeberth"
	"github.com/sean-brydon/leadline/internal/store"
)

// forwardServer is a daemon whose Shipyard is the fake one in dir (none
// when withShipyard is false), with a range of local ports of its own that
// are all free: the fake forwards nothing, so no port is ever opened.
func forwardServer(t *testing.T, withShipyard bool) (base, dir string) {
	t.Helper()
	cli := ""
	if withShipyard {
		dir = t.TempDir()
		var err error
		if cli, err = fakeberth.Write(dir); err != nil {
			t.Fatal(err)
		}
	}
	discoverOptions = func(o *discover.Options) {
		o.Processes = discover.NoProcesses()
		o.Home = t.TempDir()
		o.Berth = func() string { return cli }
		o.BerthTimeout = time.Minute
		o.ForwardPorts = discover.PortRange{First: 14400, Last: 14403}
		o.PortFree = func(int) bool { return true }
	}
	t.Cleanup(func() { discoverOptions = nil })
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, nil).Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return ts.URL, dir
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }

// ask sends a request as a page at origin would ("" for a program on this
// machine) and returns the status and the body.
func ask(t *testing.T, method, url, origin, contentType, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func forwardsRan(dir string) (opened, closed int) {
	for _, run := range fakeberth.Runs(dir) {
		switch {
		case strings.HasPrefix(run, "forward "):
			opened++
		case strings.HasPrefix(run, "unforward "):
			closed++
		}
	}
	return opened, closed
}

// A machine on a Shipyard box, from the app's side: the forward is opened
// once, found again, listed, reopened when it has gone, and closed when
// the machine is removed.
func TestShipyardForwards(t *testing.T) {
	base, dir := forwardServer(t, true)
	const app = "tauri://localhost"

	// Nothing about forwards is asked of Shipyard by a daemon starting, or
	// by the routes that only list boxes.
	if code, _ := ask(t, "GET", base+"/api/discover/shipyard", app, "", ""); code != 200 {
		t.Fatalf("GET /api/discover/shipyard: %d", code)
	}
	if runs := strings.Join(fakeberth.Runs(dir), "|"); runs != "boxes" {
		t.Fatalf("before any forward was asked for, berth ran: %q", runs)
	}

	var opened discover.OpenedForward
	code, raw := ask(t, "POST", base+"/api/shipyard/forwards", app, "application/json", `{"box":"hetzner-1"}`)
	if code != 200 || json.Unmarshal(raw, &opened) != nil {
		t.Fatalf("POST /api/shipyard/forwards: %d %s", code, raw)
	}
	if !opened.Opened || opened.ID != "f0000000" || opened.Box != "hetzner-1" || opened.Local != 14400 || opened.Remote != 4320 || opened.URL != "http://127.0.0.1:14400" || opened.State != "listening" {
		t.Errorf("opened = %+v", opened)
	}

	// The machine is used again: the same forward, and berth opened nothing.
	var again discover.OpenedForward
	code, raw = ask(t, "POST", base+"/api/shipyard/forwards", app, "application/json", `{"box":"hetzner-1","local":14400}`)
	if code != 200 || json.Unmarshal(raw, &again) != nil || again.Opened || again.Forward != opened.Forward {
		t.Errorf("reused: %d %s", code, raw)
	}
	if n, _ := forwardsRan(dir); n != 1 {
		t.Errorf("berth forward ran %d times", n)
	}

	var list discover.Forwards
	code, raw = ask(t, "GET", base+"/api/shipyard/forwards", app, "", "")
	if code != 200 || json.Unmarshal(raw, &list) != nil || len(list.Forwards) != 1 || list.Forwards[0] != opened.Forward || list.Ports != (discover.PortRange{First: 14400, Last: 14403}) {
		t.Errorf("list: %d %s", code, raw)
	}

	// It went away while the laptop slept: the next use opens it again on
	// the port the machine had, so its address is the same.
	if err := writeFile(dir+"/forwards.tsv", ""); err != nil {
		t.Fatal(err)
	}
	var back discover.OpenedForward
	code, raw = ask(t, "POST", base+"/api/shipyard/forwards", app, "application/json", `{"box":"hetzner-1","local":14400}`)
	if code != 200 || json.Unmarshal(raw, &back) != nil || !back.Opened || back.URL != opened.URL || back.ID == opened.ID {
		t.Errorf("reopened: %d %s", code, raw)
	}

	// The machine is removed: its forward is closed, and Shipyard has none left.
	var closed discover.ClosedForward
	code, raw = ask(t, "POST", base+"/api/shipyard/forwards/close", app, "application/json", `{"id":"`+back.ID+`"}`)
	if code != 200 || json.Unmarshal(raw, &closed) != nil || !closed.OK || !closed.Closed || closed.Forward == nil || closed.Forward.Local != 14400 {
		t.Errorf("closed: %d %s", code, raw)
	}
	if held := fakeberth.Forwards(dir); len(held) != 0 {
		t.Errorf("after closing, Shipyard holds %+v", held)
	}
	code, raw = ask(t, "POST", base+"/api/shipyard/forwards/close", app, "application/json", `{"id":"`+back.ID+`"}`)
	if code != 200 || json.Unmarshal(raw, &closed) != nil || !closed.OK || closed.Closed {
		t.Errorf("closed twice: %d %s", code, raw)
	}
	if opens, closes := forwardsRan(dir); opens != 2 || closes != 1 {
		t.Errorf("berth forward ran %d times and unforward %d: %q", opens, closes, fakeberth.Runs(dir))
	}
}

// Who may ask, and how. These routes change Shipyard's state, so a page
// that is not Leadline's own window is refused before berth is run, and so
// is anything that is not JSON saying it is JSON.
func TestShipyardForwardsAreForLeadlinesWindow(t *testing.T) {
	base, dir := forwardServer(t, true)
	open, shut := base+"/api/shipyard/forwards", base+"/api/shipyard/forwards/close"
	body := `{"box":"hetzner-1"}`

	// Another site, and a page on this machine that isn't Leadline's: the
	// app being debugged is one, and must not be able to open a forward.
	for _, origin := range []string{"https://evil.example", "http://localhost:3000", "http://127.0.0.1:1546", "http://localhost:15200"} {
		for _, r := range [][3]string{{"POST", open, body}, {"POST", shut, `{"id":"f0000000"}`}, {"GET", open, ""}} {
			contentType := ""
			if r[2] != "" {
				contentType = "application/json"
			}
			code, raw := ask(t, r[0], r[1], origin, contentType, r[2])
			var p problemBody
			json.Unmarshal(raw, &p)
			if code != http.StatusForbidden || (strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1")) && p.Error.Message != "Only Leadline's own window can open, list or close a Shipyard forward." {
				t.Errorf("%s %s from %s: %d %s", r[0], strings.TrimPrefix(r[1], base), origin, code, raw)
			}
		}
	}
	// Leadline's window, its dev server, and a program on this machine
	// (no Origin) get through.
	for _, origin := range []string{"tauri://localhost", "http://tauri.localhost", "http://localhost:1520", "http://127.0.0.1:1520", ""} {
		if code, raw := ask(t, "GET", open, origin, "", ""); code != 200 {
			t.Errorf("GET from %q: %d %s", origin, code, raw)
		}
		if code, raw := ask(t, "POST", open, origin, "application/json", "{not json"); code != http.StatusBadRequest {
			t.Errorf("POST from %q: %d %s", origin, code, raw)
		}
	}
	// And one more when the daemon is told of it.
	t.Setenv("LEADLINE_APP_ORIGINS", "http://localhost:1561")
	if code, _ := ask(t, "GET", open, "http://localhost:1561", "", ""); code != 200 {
		t.Errorf("an origin named in LEADLINE_APP_ORIGINS: %d", code)
	}

	// A form, and JSON that does not say it is JSON.
	resp, err := http.PostForm(open, url.Values{"box": {"hetzner-1"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a form: %s", resp.Status)
	}
	for _, at := range []string{open, shut} {
		for _, contentType := range []string{"text/plain", ""} {
			code, raw := ask(t, "POST", at, "", contentType, body)
			var p problemBody
			json.Unmarshal(raw, &p)
			if code != http.StatusUnsupportedMediaType || p.Error.Code != "not_json" {
				t.Errorf("POST %s as %q: %d %s", strings.TrimPrefix(at, base), contentType, code, raw)
			}
		}
		if code, _ := ask(t, "POST", at, "", "application/json", ""); code != http.StatusBadRequest {
			t.Errorf("POST %s with no body: %d", strings.TrimPrefix(at, base), code)
		}
		if code, _ := ask(t, "POST", at, "", "application/json", `{"box":"`+strings.Repeat("a", 8<<10)+`"}`); code != http.StatusBadRequest {
			t.Errorf("POST %s with a large body: %d", strings.TrimPrefix(at, base), code)
		}
	}
	if opens, closes := forwardsRan(dir); opens != 0 || closes != 0 {
		t.Errorf("a refused request ran berth: forward %d times, unforward %d: %q", opens, closes, fakeberth.Runs(dir))
	}
	if held := fakeberth.Forwards(dir); len(held) != 0 {
		t.Errorf("a refused request left a forward: %+v", held)
	}
}

// Refusals, in words, with a status that says whose fault it is.
func TestShipyardForwardProblems(t *testing.T) {
	base, dir := forwardServer(t, true)
	if err := writeFile(dir+"/forwards.tsv", "9f3a1c2e hetzner-1 3000 3000 listening\n"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, body string
		status     int
		code       string
		message    string
	}{
		{"/api/shipyard/forwards", `{"box":"build-box"}`, 503, "box_offline", "The box build-box is offline."},
		{"/api/shipyard/forwards", `{"box":"nowhere"}`, 404, "no_box", "Shipyard has no box named nowhere."},
		{"/api/shipyard/forwards", `{"box":"this-mac"}`, 400, "box_local", "this-mac is this computer, whose Leadline needs no forward."},
		{"/api/shipyard/forwards", `{"box":"--json"}`, 400, "bad_request", "A forward needs the name of a box."},
		{"/api/shipyard/forwards", `{}`, 400, "bad_request", "A forward needs the name of a box."},
		{"/api/shipyard/forwards/close", `{"id":"9f3a1c2e"}`, 409, "not_leadline", "That forward goes to port 3000 on hetzner-1, not to a Leadline, so Leadline leaves it alone."},
		{"/api/shipyard/forwards/close", `{"id":"--json"}`, 400, "bad_request", "That is not a forward's id."},
		{"/api/shipyard/forwards/close", `{}`, 400, "bad_request", "That is not a forward's id."},
	} {
		status, p := send(t, base+c.path, json.RawMessage(c.body), nil)
		if status != c.status || p.Error.Code != c.code || p.Error.Message != c.message {
			t.Errorf("%s %s: %d %+v, want %d %s", c.path, c.body, status, p.Error, c.status, c.code)
		}
	}
	if opens, closes := forwardsRan(dir); opens != 0 || closes != 0 {
		t.Errorf("a refusal ran berth: %q", fakeberth.Runs(dir))
	}

	// A port Shipyard finds taken, three times over.
	if err := writeFile(dir+"/forward.sh", `echo "berth: localhost:$local: local port $local is in use: listen tcp 127.0.0.1:$local: bind: address already in use" >&2; exit 1`); err != nil {
		t.Fatal(err)
	}
	status, p := send(t, base+"/api/shipyard/forwards", map[string]any{"box": "hetzner-1"}, nil)
	if status != 409 || p.Error.Code != "port_in_use" || p.Error.Message != "Port 14402 is in use." {
		t.Errorf("a taken port: %d %+v", status, p.Error)
	}

	// Without Shipyard, all three say so.
	none, _ := forwardServer(t, false)
	for _, r := range [][3]string{{"GET", "/api/shipyard/forwards", ""}, {"POST", "/api/shipyard/forwards", `{"box":"hetzner-1"}`}, {"POST", "/api/shipyard/forwards/close", `{"id":"f0000000"}`}} {
		contentType := ""
		if r[2] != "" {
			contentType = "application/json"
		}
		code, raw := ask(t, r[0], none+r[1], "", contentType, r[2])
		var p problemBody
		json.Unmarshal(raw, &p)
		if code != 404 || p.Error.Code != "shipyard_not_found" || p.Error.Message != "Shipyard is not installed on this machine." {
			t.Errorf("%s %s without Shipyard: %d %s", r[0], r[1], code, raw)
		}
	}
}

// A foreign Host never reaches these routes.
func TestShipyardForwardsAreBehindTheGuard(t *testing.T) {
	base, dir := forwardServer(t, true)
	req, _ := http.NewRequest("POST", base+"/api/shipyard/forwards", bytes.NewReader([]byte(`{"box":"hetzner-1"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "leadline.evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(fakeberth.Runs(dir)) != 0 {
		t.Errorf("a foreign Host: %s, berth ran %q", resp.Status, fakeberth.Runs(dir))
	}
}
