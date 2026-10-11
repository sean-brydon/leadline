package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sean-brydon/leadline/skills"
)

func TestSetupMarkdownIsServedForThisDaemon(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/setup.md")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("%s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	md := string(body)
	// The test server is on a port of its own: every address in the
	// instructions must be that one, not the default.
	if strings.Contains(md, skills.DefaultURL) {
		t.Error("the instructions name the default address, not this daemon's")
	}
	for _, want := range []string{
		"# Set up Leadline in this project",
		"curl -s " + ts.URL + "/api/health",
		"claude mcp add --transport http leadline " + ts.URL + "/mcp",
		"leadline run -- ",
		"## 7. Prove it",
		"## Do not",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q", want)
		}
	}
	// One source: what is served is what the package composes from the skill.
	if md != skills.SetupMarkdown(ts.URL) {
		t.Error("/setup.md is not skills.SetupMarkdown for this address")
	}
}

func TestSetupPromptPointsAtTheInstructions(t *testing.T) {
	ts := newTestServer(t)
	var out struct {
		Prompt   string `json:"prompt"`
		URL      string `json:"url"`
		SetupURL string `json:"setup_url"`
	}
	getJSON(t, ts.URL+"/api/setup/prompt", &out)
	if out.URL != ts.URL || out.SetupURL != ts.URL+"/setup.md" {
		t.Errorf("%+v", out)
	}
	if !strings.Contains(out.Prompt, out.SetupURL) || strings.Contains(out.Prompt, "\n") {
		t.Errorf("prompt: %q", out.Prompt)
	}
	// What the prompt tells an agent to fetch is really there.
	resp, err := http.Get(out.SetupURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s: %s", out.SetupURL, resp.Status)
	}
}

func TestSetupIsBehindTheGuard(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/setup.md", "/api/setup/prompt"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Host = "leadline.evil.example"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with a foreign Host: %d", path, resp.StatusCode)
		}
		// And the foreign name is never written into instructions.
		if strings.Contains(string(body), "evil.example") {
			t.Errorf("%s echoed the foreign host", path)
		}
	}
}

// forwarded asks a path the way a window on another computer does: through
// a port forwarded to this daemon, so the request names the forward's end
// ("127.0.0.1:14320") and not the port the daemon listens on.
func forwarded(t *testing.T, ts string, method, path, host, body string) string {
	t.Helper()
	req, err := http.NewRequest(method, ts+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s as %s: %s %s", method, path, host, resp.Status, raw)
	}
	return string(raw)
}

// Looked at through a forwarded port, the daemon still describes itself by
// the address it listens on: that is the one that works on its machine,
// where the prompt is pasted and the instructions are followed.
func TestSetupNamesTheDaemonsOwnAddressThroughAForward(t *testing.T) {
	ts := newTestServer(t)
	for _, host := range []string{"127.0.0.1:14320", "localhost:14320", "127.0.0.1", "[::1]:4336"} {
		var out struct {
			Prompt   string `json:"prompt"`
			URL      string `json:"url"`
			SetupURL string `json:"setup_url"`
		}
		raw := forwarded(t, ts.URL, "GET", "/api/setup/prompt", host, "")
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		if out.URL != ts.URL || out.SetupURL != ts.URL+"/setup.md" || out.Prompt != skills.SetupPrompt(ts.URL) {
			t.Errorf("through %s: %+v, want %s", host, out, ts.URL)
		}
		md := forwarded(t, ts.URL, "GET", "/setup.md", host, "")
		if md != skills.SetupMarkdown(ts.URL) {
			t.Errorf("through %s: /setup.md is not the instructions for %s", host, ts.URL)
		}
		for _, text := range []string{raw, md} {
			if strings.Contains(text, "14320") || strings.Contains(text, "4336") {
				t.Errorf("through %s: the forwarded port is named", host)
			}
		}
	}
}

// Reached at the port it listens on, the daemon keeps the name it was
// reached by: "localhost" stays "localhost".
func TestSetupKeepsTheNameItWasReachedBy(t *testing.T) {
	ts := newTestServer(t)
	port := ts.URL[strings.LastIndex(ts.URL, ":"):]
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(forwarded(t, ts.URL, "GET", "/api/setup/prompt", "localhost"+port, "")), &out); err != nil {
		t.Fatal(err)
	}
	if out.URL != "http://localhost"+port {
		t.Errorf("url = %s", out.URL)
	}
}

// A request made in process has no connection to read a port from: its
// Host is what there is.
func TestSelfURLWithoutAConnection(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:4320/setup.md", nil)
	if got := selfURL(req); got != "http://127.0.0.1:4320" {
		t.Errorf("selfURL = %s", got)
	}
}
