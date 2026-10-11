package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Reaching the Leadline on a box. A box's daemon listens on that box's
// localhost and nowhere else, so this computer reaches it through a port
// Shipyard forwards over its own encrypted connection. Three more berth
// commands are run from this file, and they are of two kinds:
//
//	berth forwards --json                the forwards Shipyard holds; reads only
//	berth forward BOX LOCAL:4320 --json  opens one, to the box's Leadline
//	berth unforward ID                   closes one
//
// The last two change Shipyard's state. They are run from OpenForward and
// CloseForward and nowhere else, and each of those is called by one route,
// when a person adds, uses or removes a machine in the app. Nothing here
// runs on a timer or because data arrived, and nothing retries by itself
// beyond trying the next free port.
//
// Shipyard does not record who opened a forward, and nor does this
// package: the answer to OpenForward says whether that call opened the
// forward or found it there, and whoever asked keeps that.

// LeadlinePort is where a Leadline daemon listens unless told otherwise,
// which is where scripts/install.sh puts one on a box.
const LeadlinePort = 4320

// The local ports a forward is given, first to last, unless
// LEADLINE_FORWARD_PORTS says otherwise.
const (
	firstForwardPort = 14320
	lastForwardPort  = 14339
)

// Forward is a port on this computer that Shipyard carries to a box's
// Leadline.
type Forward struct {
	// ID is Shipyard's own name for it, which closes it.
	ID  string `json:"id"`
	Box string `json:"box"`
	// Local is the port on this computer, Remote the one on the box.
	Local  int `json:"local"`
	Remote int `json:"remote"`
	// URL is where the box's Leadline is reached from this computer.
	URL string `json:"url"`
	// State is Shipyard's own: "listening", or "failed" when it could not
	// open the local port, with why in Error.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// OpenedForward is the forward to a box's Leadline, and whether the call
// that asked for it is the one that opened it.
type OpenedForward struct {
	Forward
	// Opened: this call ran `berth forward`. False: the forward was there
	// already, opened by an earlier call or by a person, and is reused.
	Opened bool `json:"opened"`
}

// Forwards is every forward Shipyard holds to a box's Leadline.
type Forwards struct {
	Forwards []Forward `json:"forwards"`
	// Ports is the range a new forward's local port is picked from.
	Ports     PortRange `json:"ports"`
	CheckedMs float64   `json:"checked_ms"`
}

// PortRange is a run of local ports, both ends included.
type PortRange struct {
	First int `json:"first"`
	Last  int `json:"last"`
}

// ClosedForward is what became of a forward that was asked to be closed.
type ClosedForward struct {
	OK bool `json:"ok"`
	// Closed: `berth unforward` ran and stopped it. False: Shipyard had no
	// forward of that id, so there was nothing to close.
	Closed  bool     `json:"closed"`
	Forward *Forward `json:"forward,omitempty"`
}

// A forward's id is Shipyard's: a few hex digits. Nothing that passes
// can be read as a flag.
var forwardID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// forwardPorts reads LEADLINE_FORWARD_PORTS, "FIRST-LAST", for a computer
// where the usual range is taken or a test that must keep off it. Anything
// it can't read leaves the usual range.
func forwardPorts() PortRange {
	usual := PortRange{First: firstForwardPort, Last: lastForwardPort}
	a, b, ok := strings.Cut(strings.TrimSpace(os.Getenv("LEADLINE_FORWARD_PORTS")), "-")
	if !ok {
		return usual
	}
	first, err1 := strconv.Atoi(strings.TrimSpace(a))
	last, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || first < 1024 || last > 65535 || last < first || last-first >= 256 {
		return usual
	}
	return PortRange{First: first, Last: last}
}

// portFree reports whether nothing on this computer has a local port. It
// listens there for an instant, which is what Shipyard will do; and it
// asks the port too, since on macOS a loopback listener is allowed beside
// another program's listener on every address.
func portFree(port int) bool {
	at := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", at)
	if err != nil {
		return false
	}
	ln.Close()
	conn, err := net.DialTimeout("tcp", at, 150*time.Millisecond)
	if err != nil {
		return true
	}
	conn.Close()
	return false
}

// berthForward is what `berth forwards --json` prints for a forward, and
// `berth forward --json` for the one it opened (without its state).
type berthForward struct {
	ID     string `json:"id"`
	Box    string `json:"box"`
	Local  int    `json:"local"`
	Remote int    `json:"remote"`
	State  string `json:"state"`
	Error  string `json:"error"`
}

func (b berthForward) forward() (Forward, bool) {
	if !forwardID.MatchString(b.ID) || !boxName.MatchString(b.Box) || b.Local < 1 || b.Local > 65535 || b.Remote < 1 || b.Remote > 65535 {
		return Forward{}, false
	}
	state := b.State
	if state == "" {
		state = "listening"
	}
	return Forward{ID: b.ID, Box: b.Box, Local: b.Local, Remote: b.Remote, URL: forwardURL(b.Local), State: clip(state, 32), Error: clip(b.Error, 200)}, true
}

func forwardURL(local int) string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(local))
}

// listForwards runs `berth forwards --json`: every forward Shipyard
// holds, to anything.
func (s *shipyard) listForwards(ctx context.Context, cli string) ([]Forward, error) {
	raw, err := s.run(ctx, cli, s.locationsTimeout, "forwards", "--json")
	if err != nil {
		return nil, err
	}
	var listed []berthForward
	// With none it prints [] or null.
	if err := json.Unmarshal(bytes.TrimSpace(raw), &listed); err != nil {
		return nil, &Problem{Code: "shipyard_failed", Message: "Shipyard's command line answered with something Leadline can't read."}
	}
	out := []Forward{}
	for _, b := range listed {
		if f, ok := b.forward(); ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// Forwards lists the forwards Shipyard holds to a box's Leadline: the
// ones OpenForward opened, and any a person opened by hand to the same
// port, which Shipyard does not tell apart. A forward to any other port is
// not Leadline's business and is left out.
func (s *Service) Forwards(ctx context.Context) (Forwards, error) {
	sh := s.ship
	cli := sh.find()
	if cli == "" {
		return Forwards{}, errNoShipyard
	}
	sh.forwarding.Lock()
	defer sh.forwarding.Unlock()
	all, err := sh.listForwards(ctx, cli)
	if err != nil {
		return Forwards{}, err
	}
	out := Forwards{Forwards: []Forward{}, Ports: sh.ports, CheckedMs: ms(sh.now())}
	for _, f := range all {
		if f.Remote == LeadlinePort {
			out.Forwards = append(out.Forwards, f)
		}
	}
	return out, nil
}

// OpenForward makes sure this computer has a forward to a box's Leadline,
// and returns it. One that is there already is returned as it is, whoever
// opened it: a box never gets two. Otherwise one is opened on a free local
// port from the range: `prefer` when that is free (the port the machine
// had before, so its address stays the same), else the first that is.
//
// Everything that can be checked by reading is checked first, so most
// refusals are Leadline's own, in plain words, and come before anything
// was changed. Whether a Leadline answers through the forward is for the
// caller to find out: this only opens the way.
func (s *Service) OpenForward(ctx context.Context, box string, prefer int) (OpenedForward, error) {
	bad := func(code, message, hint string) (OpenedForward, error) {
		return OpenedForward{}, &Problem{Code: code, Message: message, Hint: hint}
	}
	if !boxName.MatchString(box) {
		return bad("bad_request", "A forward needs the name of a box.", "")
	}
	listing := s.Shipyard(ctx, false)
	if !listing.Available {
		if listing.Problem != nil && listing.Installed {
			return OpenedForward{}, listing.Problem
		}
		return OpenedForward{}, errNoShipyard
	}
	var at *Box
	for i := range listing.Boxes {
		if listing.Boxes[i].Name == box {
			at = &listing.Boxes[i]
		}
	}
	if at == nil {
		return bad("no_box", fmt.Sprintf("Shipyard has no box named %s.", box), "")
	}
	if at.Local {
		return bad("box_local", fmt.Sprintf("%s is this computer, whose Leadline needs no forward.", box), "")
	}
	if p := boxAway(*at); p != nil {
		return OpenedForward{}, p
	}

	sh := s.ship
	// One at a time: two requests for one box are one forward, and the
	// second sees what the first opened.
	sh.forwarding.Lock()
	defer sh.forwarding.Unlock()
	held, err := sh.listForwards(ctx, listing.CLI)
	if err != nil {
		return OpenedForward{}, err
	}
	taken := map[int]bool{}
	for _, f := range held {
		taken[f.Local] = true
		if f.Box != box || f.Remote != LeadlinePort {
			continue
		}
		if f.State == "failed" {
			hint := fmt.Sprintf("Free the port, or close the forward with `berth unforward %s`, and try again.", f.ID)
			if f.Error != "" {
				hint += " " + shipyardSaid(f.Error, "")
			}
			return bad("forward_failed", fmt.Sprintf("Shipyard has a forward to %s on port %d, and could not open that port.", box, f.Local), hint)
		}
		return OpenedForward{Forward: f}, nil
	}

	// A port Shipyard refuses although it looked free is passed over, a
	// few times: something else may have taken it in between.
	var last *Problem
	for tries := 0; tries < 3; tries++ {
		port := sh.pickPort(prefer, taken)
		if port == 0 {
			if last != nil {
				return OpenedForward{}, last
			}
			return bad("no_port", fmt.Sprintf("No port from %d to %d is free on this computer.", sh.ports.First, sh.ports.Last),
				"Close a forward you no longer need (`berth forwards` lists them), or set LEADLINE_FORWARD_PORTS to another range.")
		}
		taken[port] = true
		stdout, stderr, err := runTask(ctx, listing.CLI, sh.forwardTimeout, []string{"forward", box, strconv.Itoa(port) + ":" + strconv.Itoa(LeadlinePort), "--json"})
		if err != nil {
			p := forwardProblem(err, stderr, box, port)
			if p.Code != "port_in_use" {
				return OpenedForward{}, p
			}
			last = p
			continue
		}
		var opened []berthForward
		if json.Unmarshal(bytes.TrimSpace(stdout), &opened) == nil && len(opened) > 0 {
			if f, ok := opened[0].forward(); ok {
				return OpenedForward{Forward: f, Opened: true}, nil
			}
		}
		// It opened and did not say so in a shape Leadline reads: the
		// listing has it.
		if again, err := sh.listForwards(ctx, listing.CLI); err == nil {
			for _, f := range again {
				if f.Box == box && f.Remote == LeadlinePort && f.Local == port {
					return OpenedForward{Forward: f, Opened: true}, nil
				}
			}
		}
		return bad("shipyard_failed", "Shipyard's command line answered with something Leadline can't read.", "")
	}
	return OpenedForward{}, last
}

// pickPort is a free local port from the range that no forward has:
// prefer when it is one, else the first. 0 when there is none.
func (s *shipyard) pickPort(prefer int, taken map[int]bool) int {
	ok := func(p int) bool { return p >= s.ports.First && p <= s.ports.Last && !taken[p] && s.free(p) }
	if ok(prefer) {
		return prefer
	}
	for p := s.ports.First; p <= s.ports.Last; p++ {
		if p != prefer && ok(p) {
			return p
		}
	}
	return 0
}

// CloseForward closes a forward by Shipyard's id for it. Only a forward
// to a box's Leadline is closed: any other is not Leadline's to touch. One
// that Shipyard no longer has is not an error, since what was wanted is
// already so.
func (s *Service) CloseForward(ctx context.Context, id string) (ClosedForward, error) {
	if !forwardID.MatchString(id) {
		return ClosedForward{}, &Problem{Code: "bad_request", Message: "That is not a forward's id."}
	}
	sh := s.ship
	cli := sh.find()
	if cli == "" {
		return ClosedForward{}, errNoShipyard
	}
	sh.forwarding.Lock()
	defer sh.forwarding.Unlock()
	held, err := sh.listForwards(ctx, cli)
	if err != nil {
		return ClosedForward{}, err
	}
	var found *Forward
	for i := range held {
		if held[i].ID == id {
			found = &held[i]
		}
	}
	if found == nil {
		return ClosedForward{OK: true}, nil
	}
	if found.Remote != LeadlinePort {
		return ClosedForward{}, &Problem{Code: "not_leadline", Message: fmt.Sprintf("That forward goes to port %d on %s, not to a Leadline, so Leadline leaves it alone.", found.Remote, found.Box),
			Hint: fmt.Sprintf("`berth unforward %s` closes it.", id)}
	}
	_, stderr, err := runTask(ctx, cli, sh.forwardTimeout, []string{"unforward", id})
	if err != nil {
		if _, ok := err.(errTimedOut); ok {
			return ClosedForward{}, &Problem{Code: "shipyard_timeout", Message: "Shipyard did not answer in time.", Hint: "The forward may be closed all the same: `berth forwards` lists what is open."}
		}
		line := berthSaid(stderr)
		if strings.Contains(strings.ToLower(line), "no forward with that id") {
			return ClosedForward{OK: true}, nil
		}
		return ClosedForward{}, &Problem{Code: "shipyard_failed", Message: "Shipyard could not close the forward.", Hint: shipyardSaid(line, "Its command line ended without saying why.")}
	}
	return ClosedForward{OK: true, Closed: true, Forward: found}, nil
}

// berthSaid is the first line berth wrote to explain itself.
func berthSaid(stderr []byte) string {
	return strings.TrimSpace(strings.TrimPrefix(firstLine(string(stderr)), "berth:"))
}

// shipyardSaid is a hint that passes on what Shipyard said, or `otherwise` when it
// said nothing.
func shipyardSaid(line, otherwise string) string {
	if line == "" {
		return otherwise
	}
	return "Shipyard said: " + clip(line, 300)
}

// forwardProblem turns a failed `berth forward` into words. A person
// pressed a button and has to know why it did not work, so what berth
// said is passed on, as a hint, behind Leadline's own sentence for the
// cases it knows.
func forwardProblem(err error, stderr []byte, box string, port int) *Problem {
	if _, ok := err.(errTimedOut); ok {
		return &Problem{Code: "shipyard_timeout", Message: "Shipyard did not answer in time.",
			Hint: "Try again: a forward that did open is found and used."}
	}
	line := berthSaid(stderr)
	low := strings.ToLower(line)
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(low, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has("is in use", "already used", "address already in use"):
		return &Problem{Code: "port_in_use", Message: fmt.Sprintf("Port %d is in use.", port), Hint: "Another program on this computer has it. Try again, and Leadline picks another."}
	case has("no paired box"):
		return &Problem{Code: "no_box", Message: fmt.Sprintf("Shipyard has no box named %s.", box)}
	case has("is offline", "i/o timeout", "connection refused", "no route to host", "unreachable", "not reachable"):
		return &Problem{Code: "box_offline", Message: fmt.Sprintf("The box %s is offline, or did not answer.", box), Hint: shipyardSaid(line, "")}
	}
	return &Problem{Code: "shipyard_failed", Message: "Shipyard could not open the forward.", Hint: shipyardSaid(line, "Its command line ended without saying why.")}
}
