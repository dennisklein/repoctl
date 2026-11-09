package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestLockWaitOnSetLockCommands reads the command reference of the design
// (docs/design/cli.html) and checks that each command whose Lock row names
// the set lock has --lock-wait ("--lock-wait D: each command that takes
// the set lock").
func TestLockWaitOnSetLockCommands(t *testing.T) {
	b, err := os.ReadFile("../../docs/design/cli.html")
	if err != nil {
		t.Skip("no design record:", err)
	}
	tags := regexp.MustCompile(`<[^>]+>`)
	reSection := regexp.MustCompile(`(?s)<section class="cmd" id="[^"]+">(.*?)</section>`)
	reHead := regexp.MustCompile(`(?s)<h4><code>repoctl ([^<]+)</code>`)
	reLock := regexp.MustCompile(`(?s)<dt>Lock</dt><dd>(.*?)</dd>`)
	a := &App{Getenv: func(string) string { return "" }}
	root := a.newRoot()
	n := 0
	for _, m := range reSection.FindAllStringSubmatch(string(b), -1) {
		head, lock := reHead.FindStringSubmatch(m[1]), reLock.FindStringSubmatch(m[1])
		if head == nil || lock == nil {
			continue
		}
		text := tags.ReplaceAllString(lock[1], "")
		if !strings.Contains(text, "set lock") || strings.Contains(text, "no set lock") || strings.HasPrefix(text, "none") {
			continue
		}
		for _, path := range expand(strings.Fields(head[1])) {
			cmd, _, err := root.Find(path)
			if err != nil || cmd == root {
				t.Errorf("repoctl %s: not in the command tree", strings.Join(path, " "))
				continue
			}
			for _, c := range leaves(cmd) {
				// mock shell takes only the host mock lock.
				if c.Name() == "shell" {
					continue
				}
				n++
				if c.Flags().Lookup("lock-wait") == nil {
					t.Errorf("%s takes the set lock (%s) but has no --lock-wait", c.CommandPath(), text)
				}
			}
		}
	}
	if n < 20 {
		t.Errorf("checked only %d commands; the design changed?", n)
	}
}

// expand turns "sign add|retire" into [sign add] [sign retire].
func expand(words []string) [][]string {
	out := [][]string{{}}
	for _, w := range words {
		alts := strings.Split(w, "|")
		var next [][]string
		for _, p := range out {
			for _, alt := range alts {
				next = append(next, append(append([]string{}, p...), alt))
			}
		}
		out = next
	}
	return out
}

// leaves gives the runnable commands below cmd, or cmd itself.
func leaves(cmd *cobra.Command) []*cobra.Command {
	if !cmd.HasSubCommands() {
		return []*cobra.Command{cmd}
	}
	var out []*cobra.Command
	for _, c := range cmd.Commands() {
		out = append(out, leaves(c)...)
	}
	return out
}

// TestSynopsisFlags reads the synopsis of each command in the design
// (docs/design/cli.html) and checks that the command has each flag that
// the synopsis names.
func TestSynopsisFlags(t *testing.T) {
	b, err := os.ReadFile("../../docs/design/cli.html")
	if err != nil {
		t.Skip("no design record:", err)
	}
	tags := regexp.MustCompile(`<[^>]+>`)
	reSyn := regexp.MustCompile(`(?s)<pre class="synopsis">(.*?)</pre>`)
	reFlag := regexp.MustCompile(`--[a-z][a-z0-9-]*`)
	a := &App{Getenv: func(string) string { return "" }}
	root := a.newRoot()
	n := 0
	for _, m := range reSyn.FindAllStringSubmatch(string(b), -1) {
		for _, line := range strings.Split(tags.ReplaceAllString(m[1], ""), "\n") {
			words := strings.Fields(line)
			if len(words) < 2 || words[0] != "repoctl" {
				continue
			}
			var path []string
			for _, w := range words[1:] {
				if strings.ContainsAny(w, "[-<.") || strings.ToUpper(w) == w {
					break
				}
				path = append(path, w)
			}
			for _, p := range expand(path) {
				cmd, _, err := root.Find(p)
				if err != nil || cmd == root {
					t.Errorf("repoctl %s: not in the command tree", strings.Join(p, " "))
					continue
				}
				for _, f := range reFlag.FindAllString(line, -1) {
					n++
					if cmd.Flag(strings.TrimPrefix(f, "--")) == nil {
						t.Errorf("%s has no %s (synopsis: %s)", cmd.CommandPath(), f, strings.TrimSpace(line))
					}
				}
			}
		}
	}
	if n < 100 {
		t.Errorf("checked only %d flags; the design changed?", n)
	}
}
