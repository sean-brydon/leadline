package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// On a Linux machine the daemon runs as a systemd user service, which
// scripts/install.sh sets up: leadlined.service in the person's own systemd,
// no root. These commands are thin wrappers over systemctl and journalctl
// for that one unit. What they add is an answer in plain words when there is
// nothing to manage: not Linux, no systemd, not installed, no user session.

const (
	serviceUnit    = "leadlined.service"
	installCommand = "curl -fsSL https://github.com/sean-brydon/leadline/releases/latest/download/install.sh | sh"
)

const serviceUsage = `usage: leadline service <status|start|stop|restart|logs> [flags]

The Leadline daemon as a background service on a Linux machine (a systemd
user service, set up by scripts/install.sh).

  status   whether it is running, its version, and whether it starts at boot
  start    start it
  stop     stop it (it starts again at the next boot, or "start")
  restart  stop and start it, as after changing its settings
  logs     what the daemon has written; -f follows, -n says how many lines
`

func runService(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, serviceUsage)
		return 2
	}
	verb := args[0]
	switch verb {
	case "status", "start", "stop", "restart", "logs":
	case "-h", "--help", "help":
		fmt.Print(serviceUsage)
		return 0
	default:
		fmt.Fprint(os.Stderr, serviceUsage)
		return 2
	}
	fset := flag.NewFlagSet("service "+verb, flag.ExitOnError)
	base := fset.String("url", envOr("LEADLINE_URL", "http://127.0.0.1:4320"), "the daemon's address")
	follow := fset.Bool("f", false, "logs: keep printing new lines until stopped")
	lines := fset.Int("n", 100, "logs: how many of the last lines to print")
	fset.Parse(args[1:])
	url := strings.TrimRight(*base, "/")

	h := thisHost()
	if why := h.problem(); why != "" {
		fmt.Fprintln(os.Stderr, why)
		// Without a service the daemon may still be running by hand, and
		// "is it up" is what status was asked.
		if verb == "status" {
			if v, ok := daemonVersion(url); ok {
				fmt.Fprintf(os.Stderr, "\nA daemon%s is answering at %s all the same, started some other way.\n", versionWords(v), url)
			}
		}
		return 1
	}

	switch verb {
	case "status":
		return h.status(url)
	case "logs":
		return h.logs(*lines, *follow)
	}
	if out, err := h.systemctl(verb, serviceUnit); err != nil {
		fmt.Fprintln(os.Stderr, h.explain(out, err))
		return 1
	}
	if verb == "stop" {
		fmt.Println("leadlined is stopped. \"leadline service start\" starts it again, and so does the next boot.")
		return 0
	}
	// systemd says "started" as soon as the process exists; the daemon
	// answers a moment later, once it has opened its database.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, ok := daemonVersion(url); ok {
			fmt.Printf("leadlined%s is running at %s\n", versionWords(v), url)
			return 0
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "leadlined was started but is not answering at %s. \"leadline service logs\" says why.\n", url)
			return 1
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func versionWords(v string) string {
	if v == "" {
		return ""
	}
	return " " + v
}

// host is what these commands need to know about the machine, as values, so
// the words in problem can be tested without a Linux machine.
type host struct {
	goos     string
	home     string
	username string
	uid      int
	// XDG_CONFIG_HOME and XDG_RUNTIME_DIR as the environment has them.
	configHome, runtimeDir string
	exists                 func(path string) bool
	hasCommand             func(name string) bool
}

func thisHost() host {
	h := host{
		goos:       runtime.GOOS,
		uid:        os.Getuid(),
		configHome: os.Getenv("XDG_CONFIG_HOME"),
		runtimeDir: os.Getenv("XDG_RUNTIME_DIR"),
		exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		hasCommand: func(name string) bool {
			_, err := exec.LookPath(name)
			return err == nil
		},
	}
	h.home, _ = os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		h.username = u.Username
	}
	if h.username == "" {
		h.username = os.Getenv("USER")
	}
	return h
}

// unitPath is where the installer writes the unit.
func (h host) unitPath() string {
	dir := h.configHome
	if dir == "" {
		dir = filepath.Join(h.home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", serviceUnit)
}

// runtime is the folder systemd's user session lives in. A login over SSH
// sets XDG_RUNTIME_DIR; `su` and `sudo -u` don't, though the folder is
// there whenever the session is (lingering, or another login).
func (h host) runtime() string {
	if h.runtimeDir != "" {
		return h.runtimeDir
	}
	return "/run/user/" + strconv.Itoa(h.uid)
}

// problem says why there is no service to manage here, in words a person
// can act on, or "" when there is one.
func (h host) problem() string {
	byHand := "Run the daemon by hand instead:\n  nohup leadlined >> ~/.leadline/leadlined.log 2>&1 &"
	switch {
	case h.goos == "darwin":
		return "On macOS the Leadline app runs the daemon, so there is no service to manage: open the app, or run leadlined in a terminal."
	case h.goos != "linux":
		return "leadline service manages a systemd user service, which only Linux has. Here, run leadlined in a terminal."
	case !h.exists("/run/systemd/system") || !h.hasCommand("systemctl"):
		return "This machine doesn't run systemd (a container, or a distribution without it), so there is no service to manage.\n" + byHand
	case !h.exists(h.unitPath()):
		return "The Leadline service isn't installed for this user: there is no " + h.unitPath() + ".\nInstall it with\n  " + installCommand + "\nor run the daemon by hand:\n  leadlined"
	case !h.exists(h.runtime()):
		return h.noSession()
	}
	return ""
}

func (h host) noSession() string {
	return "systemd has no session for " + h.username + " right now (no " + h.runtime() + "), so its services can't be reached.\n" +
		"Log in over SSH rather than through su or sudo, or let your services run without a login:\n  sudo loginctl enable-linger " + h.username
}

// command is systemctl or journalctl for the user's own systemd.
func (h host) command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, append([]string{"--user"}, args...)...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+h.runtime())
	return cmd
}

func (h host) systemctl(args ...string) (string, error) {
	out, err := h.command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// explain turns a failed systemctl into something to do about it.
func (h host) explain(out string, err error) string {
	if strings.Contains(out, "Failed to connect to bus") || strings.Contains(out, "Failed to connect to user scope bus") {
		return h.noSession()
	}
	if out == "" {
		out = err.Error()
	}
	return out + "\n\"leadline service logs\" shows what the daemon wrote."
}

func (h host) status(url string) int {
	// is-active and is-enabled exit non-zero for "inactive" and
	// "disabled", which are answers, not failures.
	active, err := h.systemctl("is-active", serviceUnit)
	if strings.Contains(active, "Failed to connect") {
		fmt.Fprintln(os.Stderr, h.explain(active, err))
		return 1
	}
	enabled, _ := h.systemctl("is-enabled", serviceUnit)
	fmt.Printf("service  %s, %s\n", active, enabled)

	if v, ok := daemonVersion(url); ok {
		fmt.Printf("daemon   answering at %s%s\n", url, versionWords(v))
	} else {
		fmt.Printf("daemon   not answering at %s\n", url)
	}

	// A user's services stop with their last session and start with their
	// first, unless the user "lingers": then they start at boot.
	linger, lerr := exec.Command("loginctl", "show-user", h.username, "--property=Linger", "--value").Output()
	switch {
	case lerr != nil:
		fmt.Println("at boot  unknown: loginctl did not answer")
	case strings.TrimSpace(string(linger)) == "yes":
		fmt.Println("at boot  starts by itself (lingering is on)")
	default:
		fmt.Printf("at boot  no: it runs only while %s is logged in. To start it at boot:\n           sudo loginctl enable-linger %s\n", h.username, h.username)
	}

	fmt.Println()
	cmd := h.command("systemctl", "status", serviceUnit, "--no-pager", "--lines=5")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Run()
	if active != "active" {
		return 3 // what systemctl status answers for a unit that is not running
	}
	return 0
}

func (h host) logs(lines int, follow bool) int {
	if !h.hasCommand("journalctl") {
		fmt.Fprintln(os.Stderr, "journalctl is not installed, so the service's log can't be read. systemctl --user status "+serviceUnit+" shows its last lines.")
		return 1
	}
	args := []string{"--unit", serviceUnit, "--lines", strconv.Itoa(lines), "--no-pager"}
	if follow {
		args = append(args, "--follow")
	}
	cmd := h.command("journalctl", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "leadline:", err)
		return 1
	}
	return 0
}
