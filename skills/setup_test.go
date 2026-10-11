package skills

import (
	"strings"
	"testing"
)

func TestSetupMarkdownIsTheSkill(t *testing.T) {
	raw, err := FS.ReadFile(setupSkill)
	if err != nil {
		t.Fatal(err)
	}
	skill := stripFrontmatter(string(raw))
	md := SetupMarkdown(DefaultURL)

	if strings.Contains(md, "\n---\n") || strings.Contains(md, "description:") {
		t.Error("the skill's frontmatter is in /setup.md")
	}
	if n := strings.Count(md, "\n# ") + strings.Count(md[:2], "# "); n != 1 {
		t.Errorf("want one title, got %d", n)
	}
	// Every section of the skill is served, word for word: the two cannot
	// drift, because one is made from the other.
	sections := strings.Split(skill, "\n## ")[1:]
	if len(sections) < 7 {
		t.Fatalf("the skill has %d sections; has its layout changed?", len(sections))
	}
	for _, section := range sections {
		if !strings.Contains(md, "\n## "+section) {
			t.Errorf("section missing from /setup.md: %q", strings.SplitN(section, "\n", 2)[0])
		}
	}
}

func TestSetupMarkdownUsesTheDaemonsAddress(t *testing.T) {
	md := SetupMarkdown("http://127.0.0.1:4999/")
	if strings.Contains(md, DefaultURL) {
		t.Error("the default address is still in instructions served for another")
	}
	for _, want := range []string{
		"curl -s http://127.0.0.1:4999/api/health",
		"claude mcp add --transport http leadline http://127.0.0.1:4999/mcp",
		"codex mcp add leadline --url http://127.0.0.1:4999/mcp",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The parts an agent must not be able to miss.
func TestSetupMarkdownCoversWhatAnAgentNeeds(t *testing.T) {
	md := SetupMarkdown(DefaultURL)
	for _, want := range []string{
		"leadline run -- ",
		"leadline doctor",
		"leadline skills",
		"@leadline/node",
		"opentelemetry-instrument",
		"## 7. Prove it",
		"## Do not",
		"Touch production",
		"Add a second provider",
		"/api/summary",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestSetupPrompt(t *testing.T) {
	got := SetupPrompt("http://127.0.0.1:4999/")
	if !strings.Contains(got, "curl -s http://127.0.0.1:4999/setup.md") {
		t.Errorf("prompt does not fetch the instructions: %q", got)
	}
	if strings.Contains(got, "\n") || len(got) > 200 {
		t.Errorf("the prompt must stay one short line: %q", got)
	}
	if !strings.Contains(SetupPrompt(""), DefaultURL) {
		t.Error("an empty address should fall back to the default")
	}
}
