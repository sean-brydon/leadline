package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/runner"
	"github.com/sean-brydon/leadline/skills"
)

// runPrompt prints the one line to paste into a coding agent. The daemon
// is asked for it, so the line names the address the daemon really has;
// with no daemon the same line is made here, with a word to say so.
func runPrompt(args []string) error {
	fset := flag.NewFlagSet("prompt", flag.ExitOnError)
	url := fset.String("url", envOr("LEADLINE_URL", runner.DefaultURL), "the daemon's address")
	fset.Parse(args)
	base := strings.TrimRight(*url, "/")

	prompt, err := daemonPrompt(base)
	if err != nil {
		prompt = skills.SetupPrompt(base)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if runner.NewDaemon(base).Healthy(ctx) {
			fmt.Fprintf(os.Stderr, "leadline: the Leadline at %s is too old to serve the instructions this asks for. Update it first.\n", base)
		} else {
			fmt.Fprintf(os.Stderr, "leadline: nothing answers at %s. Start Leadline before giving an agent this:\n", base)
		}
	}
	fmt.Println(prompt)
	return nil
}

func daemonPrompt(base string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/setup/prompt", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Prompt string `json:"prompt"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Prompt == "" {
		return "", fmt.Errorf("no prompt in the answer")
	}
	return out.Prompt, nil
}
