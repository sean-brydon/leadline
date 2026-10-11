package discover

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean-brydon/leadline/internal/discover/fakeberth"
)

// A range of local ports no test opens: the fake berth forwards nothing,
// and whether a port is free is the test's to say.
var testPorts = PortRange{First: 14400, Last: 14403}

// forwarder is a Service whose berth is the fake one, with ports of its
// own and every one of them free unless `busy` names it.
func forwarder(t *testing.T, busy ...int) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	cli, err := fakeberth.Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	taken := map[int]bool{}
	for _, p := range busy {
		taken[p] = true
	}
	return New(Options{Processes: NoProcesses(), Berth: func() string { return cli }, BerthTimeout: time.Minute,
		ForwardPorts: testPorts, PortFree: func(p int) bool { return !taken[p] }}), dir
}

// How often the fake was run with a command.
func ranCount(dir, command string) int {
	n := 0
	for _, r := range fakeberth.Runs(dir) {
		if r == command || strings.HasPrefix(r, command+" ") {
			n++
		}
	}
	return n
}

// A forward is opened once, found there the next time, and opened again
// on the same port once it has gone.
func TestForwardOpenedOnceThenReusedThenReopened(t *testing.T) {
	s, dir := forwarder(t)
	ctx := context.Background()

	got, err := s.OpenForward(ctx, "hetzner-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := OpenedForward{Forward: Forward{ID: "f0000000", Box: "hetzner-1", Local: 14400, Remote: 4320, URL: "http://127.0.0.1:14400", State: "listening"}, Opened: true}
	if got != want {
		t.Errorf("opened:\n got %+v\nwant %+v", got, want)
	}
	// What berth was run with, in order: it read twice, then opened one.
	if runs := strings.Join(fakeberth.Runs(dir), "|"); runs != "boxes|forwards|forward hetzner-1 14400:4320" {
		t.Errorf("runs = %q", runs)
	}

	// Asked again, and again with the port it had: the same forward, not a second.
	for _, prefer := range []int{0, 14400, 14402} {
		again, err := s.OpenForward(ctx, "hetzner-1", prefer)
		if err != nil {
			t.Fatal(err)
		}
		want.Opened = false
		if again != want {
			t.Errorf("reused (prefer %d):\n got %+v\nwant %+v", prefer, again, want)
		}
	}
	if n := ranCount(dir, "forward"); n != 1 {
		t.Errorf("berth forward ran %d times for one box", n)
	}
	if held := fakeberth.Forwards(dir); len(held) != 1 {
		t.Errorf("Shipyard holds %+v", held)
	}

	// It went away (Shipyard was told to forget it): the next use opens it
	// again, on the port the machine had, under a new id.
	set(t, dir, "forwards.tsv", "")
	back, err := s.OpenForward(ctx, "hetzner-1", 14400)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Opened || back.Local != 14400 || back.ID == want.ID || back.URL != want.URL {
		t.Errorf("reopened = %+v", back)
	}
	if n := ranCount(dir, "forward"); n != 2 {
		t.Errorf("berth forward ran %d times", n)
	}
}

// A forward someone already has to the box's Leadline is used as it is,
// whatever port it is on; one to another port of the box is not it.
func TestForwardSomeoneElseOpenedIsReused(t *testing.T) {
	s, dir := forwarder(t)
	set(t, dir, "forwards.tsv", "9f3a1c2e hetzner-1 3000 3000 listening\nab12cd34 hetzner-1 15000 4320 listening\n")
	got, err := s.OpenForward(context.Background(), "hetzner-1", 14401)
	if err != nil {
		t.Fatal(err)
	}
	if got.Opened || got.ID != "ab12cd34" || got.Local != 15000 || got.URL != "http://127.0.0.1:15000" {
		t.Errorf("got %+v", got)
	}
	if n := ranCount(dir, "forward"); n != 0 {
		t.Errorf("berth forward ran %d times beside a forward that was there", n)
	}
}

// The local port: the one asked for when it is free and in the range, else
// the first that no forward has and nothing listens on.
func TestForwardPortChoice(t *testing.T) {
	cases := []struct {
		name   string
		busy   []int
		held   string
		prefer int
		want   int
	}{
		{name: "the first of the range", want: 14400},
		{name: "the one it had before", prefer: 14402, want: 14402},
		{name: "not one outside the range", prefer: 4320, want: 14400},
		{name: "not one a program has", busy: []int{14400, 14401}, want: 14402},
		{name: "not the one it had, now that a program has it", busy: []int{14402}, prefer: 14402, want: 14400},
		{name: "not one another box's forward has", held: "aa000001 build-box 14400 4320 listening\naa000002 build-box 14401 9000 listening\n", want: 14402},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, dir := forwarder(t, c.busy...)
			set(t, dir, "forwards.tsv", c.held)
			got, err := s.OpenForward(context.Background(), "hetzner-1", c.prefer)
			if err != nil {
				t.Fatal(err)
			}
			if got.Local != c.want || !got.Opened {
				t.Errorf("port %d (opened %v), want %d", got.Local, got.Opened, c.want)
			}
		})
	}
}

// Every way opening can fail, in words, and whether berth was asked to
// open anything on the way.
func TestForwardErrors(t *testing.T) {
	cases := []struct {
		name          string
		box           string
		busy          []int
		arrange       func(t *testing.T, dir string)
		noShipyard    bool
		timeout       time.Duration
		code, message string
		hint          string
		// opens: how many times berth forward was run.
		opens int
	}{
		{name: "Shipyard isn't installed", box: "hetzner-1", noShipyard: true,
			code: "shipyard_not_found", message: "Shipyard is not installed on this machine."},
		{name: "no such box", box: "nowhere",
			code: "no_box", message: "Shipyard has no box named nowhere."},
		{name: "not a box's name", box: "--json",
			code: "bad_request", message: "A forward needs the name of a box."},
		{name: "the box is offline", box: "build-box",
			code: "box_offline", message: "The box build-box is offline.", hint: "Shipyard said: dial tcp: i/o timeout"},
		{name: "the box is connecting", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "boxes.json", `[{"name":"hetzner-1","state":"connecting"}]`)
		}, code: "box_offline", message: "Shipyard is still connecting to the box hetzner-1.", hint: "Try again in a moment."},
		{name: "the box no longer trusts this computer", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "boxes.json", `[{"name":"hetzner-1","state":"untrusted"}]`)
		}, code: "box_offline", message: "The box hetzner-1 no longer trusts this computer.", hint: "Pair it again in Shipyard."},
		{name: "the box is this computer", box: "this-mac",
			code: "box_local", message: "this-mac is this computer, whose Leadline needs no forward."},
		{name: "every port of the range is taken", box: "hetzner-1", busy: []int{14400, 14401, 14402, 14403},
			code: "no_port", message: "No port from 14400 to 14403 is free on this computer.",
			hint: "Close a forward you no longer need (`berth forwards` lists them), or set LEADLINE_FORWARD_PORTS to another range."},
		{name: "Shipyard finds the port in use, and the next, and the next", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "forward.sh", `echo "berth: localhost:$local: local port $local is in use: listen tcp 127.0.0.1:$local: bind: address already in use" >&2; exit 1`)
		}, code: "port_in_use", message: "Port 14402 is in use.", hint: "Another program on this computer has it. Try again, and Leadline picks another.", opens: 3},
		{name: "the forward is there and Shipyard could not open its port", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "forwards.tsv", "ab12cd34 hetzner-1 14401 4320 failed local port 14401 is in use: listen tcp 127.0.0.1:14401: bind: address already in use\n")
		}, code: "forward_failed", message: "Shipyard has a forward to hetzner-1 on port 14401, and could not open that port.",
			hint: "Free the port, or close the forward with `berth unforward ab12cd34`, and try again. Shipyard said: local port 14401 is in use: listen tcp 127.0.0.1:14401: bind: address already in use"},
		{name: "the box went away between the listing and the forward", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "forward.sh", `echo 'berth: localhost:14400: no paired box named "hetzner-1"' >&2; exit 1`)
		}, code: "no_box", message: "Shipyard has no box named hetzner-1.", opens: 1},
		{name: "Shipyard fails and says why", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "forward.sh", `echo 'berth: the laptop agent is not running: dial unix /tmp/agent.sock: connect: no such file' >&2; exit 1`)
		}, code: "shipyard_failed", message: "Shipyard could not open the forward.",
			hint: "Shipyard said: the laptop agent is not running: dial unix /tmp/agent.sock: connect: no such file", opens: 1},
		{name: "Shipyard fails and says nothing", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "forward.sh", `exit 3`)
		}, code: "shipyard_failed", message: "Shipyard could not open the forward.", hint: "Its command line ended without saying why.", opens: 1},
		{name: "Shipyard does not answer in time", box: "hetzner-1", timeout: 300 * time.Millisecond, arrange: func(t *testing.T, dir string) {
			set(t, dir, "forward.sh", `sleep 5`)
		}, code: "shipyard_timeout", message: "Shipyard did not answer in time.", hint: "Try again: a forward that did open is found and used.", opens: 1},
		{name: "the forwards can't be listed", box: "hetzner-1", arrange: func(t *testing.T, dir string) {
			set(t, dir, "forwards.tsv", "bad\"id hetzner-1 x y\n")
		}, code: "shipyard_failed", message: "Shipyard's command line answered with something Leadline can't read."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, dir := forwarder(t, c.busy...)
			if c.noShipyard {
				s.ship.find = func() string { return "" }
			}
			if c.arrange != nil {
				c.arrange(t, dir)
			}
			if c.timeout > 0 {
				// The listings ran with time to spare; only the forward is hurried.
				s.Shipyard(context.Background(), false)
				s.ship.forwardTimeout = c.timeout
			}
			got, err := s.OpenForward(context.Background(), c.box, 0)
			p := asProblem(err)
			if err == nil || p.Code != c.code || p.Message != c.message || p.Hint != c.hint {
				t.Errorf("got %+v, %+v\nwant %s: %q (%q)", got, p, c.code, c.message, c.hint)
			}
			if got != (OpenedForward{}) {
				t.Errorf("a refusal came with a forward: %+v", got)
			}
			if n := ranCount(dir, "forward"); n != c.opens {
				t.Errorf("berth forward ran %d times, want %d: %q", n, c.opens, fakeberth.Runs(dir))
			}
			// A refusal leaves nothing open.
			for _, f := range fakeberth.Forwards(dir) {
				if strings.HasPrefix(f.ID, "f0") {
					t.Errorf("left open: %+v", f)
				}
			}
		})
	}
}

// A port Shipyard refuses is passed over for the next one that is free.
func TestForwardTriesTheNextPort(t *testing.T) {
	s, dir := forwarder(t)
	set(t, dir, "forward.sh", `if [ "$local" = 14400 ]; then echo "berth: localhost:$local: local port $local is already used on ::1 by another program" >&2; exit 1; fi`)
	got, err := s.OpenForward(context.Background(), "hetzner-1", 0)
	if err != nil || got.Local != 14401 || !got.Opened {
		t.Fatalf("got %+v, %v", got, err)
	}
	if runs := strings.Join(fakeberth.Runs(dir), "|"); runs != "boxes|forwards|forward hetzner-1 14400:4320|forward hetzner-1 14401:4320" {
		t.Errorf("runs = %q", runs)
	}
}

// Two requests for one box at once are one forward.
func TestForwardTwoAtOnceIsOne(t *testing.T) {
	s, dir := forwarder(t)
	done := make(chan OpenedForward, 2)
	for range 2 {
		go func() {
			got, err := s.OpenForward(context.Background(), "hetzner-1", 0)
			if err != nil {
				t.Error(err)
			}
			done <- got
		}()
	}
	a, b := <-done, <-done
	if a.ID != b.ID || a.Opened == b.Opened {
		t.Errorf("two requests: %+v and %+v", a, b)
	}
	if n := ranCount(dir, "forward"); n != 1 {
		t.Errorf("berth forward ran %d times", n)
	}
}

// Listing is of the forwards to a Leadline, and closing is of one of
// those: gone from Shipyard afterwards, and no error the second time.
func TestForwardsListedAndClosed(t *testing.T) {
	s, dir := forwarder(t)
	ctx := context.Background()
	set(t, dir, "forwards.tsv", "9f3a1c2e hetzner-1 3000 3000 listening\n")
	opened, err := s.OpenForward(ctx, "hetzner-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.Forwards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Forwards) != 1 || list.Forwards[0] != opened.Forward || list.Ports != testPorts || list.CheckedMs == 0 {
		t.Errorf("list = %+v", list)
	}
	// As JSON: the shape the app is written against.
	raw, _ := json.Marshal(opened)
	if string(raw) != `{"id":"f0000000","box":"hetzner-1","local":14400,"remote":4320,"url":"http://127.0.0.1:14400","state":"listening","opened":true}` {
		t.Errorf("JSON = %s", raw)
	}

	// A forward to something else is not Leadline's to close.
	_, err = s.CloseForward(ctx, "9f3a1c2e")
	if p := asProblem(err); err == nil || p.Code != "not_leadline" || p.Message != "That forward goes to port 3000 on hetzner-1, not to a Leadline, so Leadline leaves it alone." {
		t.Errorf("closing another forward: %+v", p)
	}
	if _, err = s.CloseForward(ctx, "-f"); asProblem(err).Code != "bad_request" {
		t.Errorf("closing a flag: %v", err)
	}
	if n := ranCount(dir, "unforward"); n != 0 {
		t.Fatalf("berth unforward ran %d times for refusals", n)
	}

	closed, err := s.CloseForward(ctx, opened.ID)
	if err != nil || !closed.OK || !closed.Closed || closed.Forward == nil || *closed.Forward != opened.Forward {
		t.Errorf("closed = %+v, %v", closed, err)
	}
	if held := fakeberth.Forwards(dir); len(held) != 1 || held[0].ID != "9f3a1c2e" {
		t.Errorf("Shipyard still holds %+v", held)
	}
	// Again: there is nothing to close, which is what was wanted.
	closed, err = s.CloseForward(ctx, opened.ID)
	if err != nil || !closed.OK || closed.Closed || closed.Forward != nil {
		t.Errorf("closed again = %+v, %v", closed, err)
	}
	if n := ranCount(dir, "unforward"); n != 1 {
		t.Errorf("berth unforward ran %d times", n)
	}
	if list, _ = s.Forwards(ctx); len(list.Forwards) != 0 || list.Forwards == nil {
		t.Errorf("list after closing = %+v", list)
	}
}

func TestForwardCloseErrors(t *testing.T) {
	s, dir := forwarder(t)
	ctx := context.Background()
	opened, err := s.OpenForward(ctx, "hetzner-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	set(t, dir, "unforward.sh", `echo 'berth: agent replied 500 Internal Server Error' >&2; exit 1`)
	_, err = s.CloseForward(ctx, opened.ID)
	if p := asProblem(err); err == nil || p.Code != "shipyard_failed" || p.Message != "Shipyard could not close the forward." || p.Hint != "Shipyard said: agent replied 500 Internal Server Error" {
		t.Errorf("a failure: %+v", p)
	}
	// Gone between the listing and the command: the same as closed.
	set(t, dir, "unforward.sh", `box=''`)
	if closed, err := s.CloseForward(ctx, opened.ID); err != nil || !closed.OK || closed.Closed {
		t.Errorf("gone meanwhile: %+v, %v", closed, err)
	}
	set(t, dir, "unforward.sh", `sleep 5`)
	s.ship.forwardTimeout = 300 * time.Millisecond
	_, err = s.CloseForward(ctx, opened.ID)
	if p := asProblem(err); err == nil || p.Code != "shipyard_timeout" || p.Message != "Shipyard did not answer in time." {
		t.Errorf("a timeout: %+v", p)
	}

	none, _ := forwarder(t)
	none.ship.find = func() string { return "" }
	if _, err := none.CloseForward(ctx, "f0000000"); asProblem(err).Code != "shipyard_not_found" {
		t.Errorf("no Shipyard: %v", err)
	}
	if _, err := none.Forwards(ctx); asProblem(err).Code != "shipyard_not_found" {
		t.Errorf("no Shipyard: %v", err)
	}
}

// The range can be moved, and nothing unreadable moves it.
func TestForwardPortsFromEnv(t *testing.T) {
	usual := PortRange{First: 14320, Last: 14339}
	for value, want := range map[string]PortRange{
		"":                usual,
		"15000-15009":     {First: 15000, Last: 15009},
		" 15000 - 15000 ": {First: 15000, Last: 15000},
		"15009-15000":     usual,
		"80-90":           usual,
		"15000":           usual,
		"a-b":             usual,
		"1024-65535":      usual,
		"60000-70000":     usual,
	} {
		t.Setenv("LEADLINE_FORWARD_PORTS", value)
		if got := forwardPorts(); got != want {
			t.Errorf("LEADLINE_FORWARD_PORTS=%q: %+v, want %+v", value, got, want)
		}
	}
}

// A port is free when nothing listens on it.
func TestPortFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if portFree(port) {
		t.Errorf("port %d is free while this test listens on it", port)
	}
	ln.Close()
	if !portFree(port) {
		t.Errorf("port %d is not free once nothing listens on it", port)
	}
}

// No berth command but the ones Leadline is allowed is ever built here:
// a forward is opened to the Leadline port and to nothing else.
func TestForwardArguments(t *testing.T) {
	s, dir := forwarder(t)
	// The fake writes what it was run with to runs.log; a wrapper keeps every argument.
	cli := dir + "/berth"
	real := dir + "/berth-real"
	if err := os.Rename(cli, real); err != nil {
		t.Fatal(err)
	}
	set(t, dir, "berth", "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> '"+dir+"/args.log'\necho -- >> '"+dir+"/args.log'\nexec '"+real+"' \"$@\"\n")
	if err := os.Chmod(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	opened, err := s.OpenForward(context.Background(), "hetzner-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseForward(context.Background(), opened.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dir + "/args.log")
	want := "boxes\n--json\n--\nforwards\n--json\n--\nforward\nhetzner-1\n14400:4320\n--json\n--\nforwards\n--json\n--\nunforward\nf0000000\n--\n"
	if string(b) != want {
		t.Errorf("berth was run with:\n%s\nwant:\n%s", b, want)
	}
}
