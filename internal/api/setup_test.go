package api

import (
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
