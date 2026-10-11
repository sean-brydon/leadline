package main

import (
	"strings"
	"testing"
)

// What `leadline service` says when there is nothing to manage: each case
// names the reason and something to do about it.
func TestServiceProblem(t *testing.T) {
	linux := func(present ...string) host {
		return host{
			goos: "linux", home: "/home/dev", username: "dev", uid: 1000,
			exists: func(path string) bool {
				for _, p := range present {
					if p == path {
						return true
					}
				}
				return false
			},
			hasCommand: func(string) bool { return true },
		}
	}
	const unit = "/home/dev/.config/systemd/user/leadlined.service"

	cases := []struct {
		name string
		h    host
		want []string
	}{
		{"macOS", host{goos: "darwin"}, []string{"the Leadline app runs the daemon"}},
		{"no systemd", linux(), []string{"doesn't run systemd", "nohup leadlined"}},
		{"not installed", linux("/run/systemd/system"), []string{"isn't installed", unit, "install.sh"}},
		{"no session", linux("/run/systemd/system", unit), []string{"no /run/user/1000", "sudo loginctl enable-linger dev"}},
		{"ready", linux("/run/systemd/system", unit, "/run/user/1000"), nil},
	}
	for _, c := range cases {
		got := c.h.problem()
		if c.want == nil && got != "" {
			t.Errorf("%s: problem = %q, want none", c.name, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q is missing %q", c.name, got, w)
			}
		}
	}

	noSystemctl := linux("/run/systemd/system")
	noSystemctl.hasCommand = func(string) bool { return false }
	if got := noSystemctl.problem(); !strings.Contains(got, "doesn't run systemd") {
		t.Errorf("no systemctl: %q", got)
	}
}

func TestServiceUnitPathFollowsXDG(t *testing.T) {
	h := host{home: "/home/dev", configHome: "/srv/config"}
	if got := h.unitPath(); got != "/srv/config/systemd/user/leadlined.service" {
		t.Errorf("unitPath = %q", got)
	}
	h = host{uid: 1000, runtimeDir: "/tmp/run"}
	if got := h.runtime(); got != "/tmp/run" {
		t.Errorf("runtime = %q", got)
	}
}
