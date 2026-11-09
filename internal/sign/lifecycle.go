package sign

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// The commands that change a key state (signing.html#lifecycle).
const (
	CmdRetire   = "retire"
	CmdActivate = "activate"
	CmdRemove   = "remove"
	CmdRevoke   = "revoke"
)

// NewState gives the state that a lifecycle command sets.
func NewState(cmd string) string {
	switch cmd {
	case CmdRetire:
		return config.KeyRetiring
	case CmdActivate:
		return config.KeyActive
	case CmdRemove:
		return config.KeyRemoved
	case CmdRevoke:
		return config.KeyRevoked
	}
	return ""
}

// CommitMessage gives the set commit of a lifecycle command, for example
// "Retire key hpc-repo 9F2C…".
func CommitMessage(cmd, id, fpr string) string {
	verb := map[string]string{CmdRetire: "Retire", CmdActivate: "Activate", CmdRemove: "Remove", CmdRevoke: "Revoke", "add": "Trust"}[cmd]
	return fmt.Sprintf("%s key %s %s", verb, id, fpr)
}

// CheckTransition checks that the command allows the state of the key:
// else sign.wrong-state (exit 1) with the allowed command.
func CheckTransition(cmd string, k *TrustedKey) error {
	wrong := func(format string, args ...any) *api.Error {
		return api.Errorf(api.PSignWrongState, "sign %s %s: "+format, append([]any{cmd, k.ID}, args...)...)
	}
	switch k.State {
	case config.KeyRevoked:
		return wrong("the key is revoked; a revoked key is final").
			WithHint("make a new key and add it", "repoctl sign add NEW-ID --fingerprint FPR")
	case config.KeyRemoved:
		if cmd == CmdRevoke {
			return nil
		}
		return wrong("the key is removed; a removed key does not come back").
			WithHint("make a new key and add it", "repoctl sign add NEW-ID --fingerprint FPR")
	}
	switch cmd {
	case CmdRetire:
		if k.State != config.KeyActive {
			return wrong("the key is %s; only an active key retires", k.State).
				WithHint("remove the retiring key after the re-sign", "repoctl sign remove "+k.ID)
		}
	case CmdActivate:
		if k.State != config.KeyRetiring {
			return wrong("the key is %s; only a retiring key becomes active again", k.State)
		}
	case CmdRemove:
		if k.State != config.KeyRetiring {
			return wrong("the key is %s; retire the key first", k.State).
				WithHint("retire the key, re-sign its RPMs, then remove it", "repoctl sign retire "+k.ID)
		}
	case CmdRevoke:
	default:
		return fmt.Errorf("sign: unknown lifecycle command %q", cmd)
	}
	return nil
}

// TrustKeyChange is one added or removed key of a trust change.
type TrustKeyChange struct {
	// ID is the id in sign.keys, "" for a key outside sign.keys.
	ID          string
	Fingerprint string
	UID         string
	// Commit is the set commit that added keys/ID.asc, Author its author,
	// SignedBy the key id that signed it.
	Commit   string
	Author   string
	SignedBy string
}

// TrustChange is the change of the trust set of a publish: the keys that
// the new key file of the release package adds or removes, and the changed
// lines of its .repo files (signing.html#trust-block).
type TrustChange struct {
	// Release is the NVR of the release package of the publish.
	Release string
	Added   []TrustKeyChange
	Removed []TrustKeyChange
	// RepoLines are the changed lines of the .repo files, with a + or -
	// prefix.
	RepoLines []string
}

// Empty reports a publish without a trust change.
func (c *TrustChange) Empty() bool {
	return c == nil || len(c.Added) == 0 && len(c.Removed) == 0 && len(c.RepoLines) == 0
}

// AddedIDs gives the ids that the human types to confirm the change.
func (c *TrustChange) AddedIDs() []string {
	var out []string
	for _, a := range c.Added {
		id := a.ID
		if id == "" {
			id = a.Fingerprint
		}
		out = append(out, id)
	}
	return out
}

// ComputeTrustChange compares the key file of the base (in the hands of
// the clients) with the key file of the release package of the publish.
// setDir names the set repository for the commit of each added key; "" skips
// the commit facts.
func ComputeTrustChange(ctx context.Context, trust *Trust, setDir, release string, oldKeyfile, newKeyfile []byte) (*TrustChange, error) {
	oldF, err := KeysOfKeyfile(oldKeyfile)
	if err != nil {
		return nil, fmt.Errorf("the key file of the base: %w", err)
	}
	newF, err := KeysOfKeyfile(newKeyfile)
	if err != nil {
		return nil, fmt.Errorf("the new key file: %w", err)
	}
	in := func(l []string, f string) bool {
		for _, x := range l {
			if x == f {
				return true
			}
		}
		return false
	}
	c := &TrustChange{Release: release}
	describe := func(fpr string) TrustKeyChange {
		ch := TrustKeyChange{Fingerprint: fpr}
		if k := trust.ByFingerprint(fpr); k != nil {
			ch.ID, ch.UID = k.ID, k.UID()
			if setDir != "" {
				ch.Commit, ch.Author, ch.SignedBy = keyCommit(ctx, trust, setDir, k.Path())
			}
		}
		return ch
	}
	for _, f := range newF {
		if !in(oldF, f) {
			c.Added = append(c.Added, describe(f))
		}
	}
	for _, f := range oldF {
		if !in(newF, f) {
			c.Removed = append(c.Removed, describe(f))
		}
	}
	return c, nil
}

// keyCommit gives the set commit that added the key file, its author and
// the key id of its signature.
func keyCommit(ctx context.Context, trust *Trust, setDir, path string) (commit, author, signedBy string) {
	out, err := git.New(setDir).Run(ctx, "log", "--diff-filter=A", "--format=%H%x00%an <%ae>%x00%GK", "--", path)
	if err != nil || out == "" {
		return "", "", ""
	}
	lines := strings.Split(out, "\n")
	f := strings.Split(lines[len(lines)-1], "\x00")
	if len(f) < 3 {
		return "", "", ""
	}
	commit, author = f[0], f[1]
	if f[2] != "" {
		if k := trust.ForIssuer(f[2]); k != nil {
			signedBy = k.ID
		} else {
			signedBy = f[2]
		}
	}
	return commit, author, signedBy
}

// Lines gives the trust block of the publish summary.
func (c *TrustChange) Lines() []string {
	var out []string
	what := "the key file"
	if len(c.Added) == 0 && len(c.Removed) == 0 {
		what = "the .repo file"
	}
	out = append(out, fmt.Sprintf("trust    %s changes %s of all clients", c.Release, what))
	pad := "         "
	list := func(sign string, l []TrustKeyChange) {
		if len(l) == 0 {
			out = append(out, pad+sign+" none")
			return
		}
		for _, k := range l {
			id := k.ID
			if id == "" {
				id = "(not in sign.keys)"
			}
			out = append(out, fmt.Sprintf("%s%s %-8s  %s", pad, sign, id, GroupFingerprint(k.Fingerprint)))
			if k.Commit != "" {
				line := fmt.Sprintf("%s           added in set commit %s by %s", pad, api.ShortCommit(k.Commit), k.Author)
				if k.SignedBy != "" {
					line += ", signed by " + k.SignedBy
				} else {
					line += ", not signed"
				}
				out = append(out, line)
			}
		}
	}
	list("+", c.Added)
	list("-", c.Removed)
	for _, l := range c.RepoLines {
		out = append(out, pad+l)
	}
	for _, k := range c.Added {
		if k.ID != "" {
			out = append(out, fmt.Sprintf("%s%s cannot sign until \"repoctl sign deployed %s\" (gate 2)", pad, k.ID, k.ID))
		}
	}
	return out
}

// DiffLines gives the lines that b adds to a (+) and removes from a (-),
// for the .repo lines of the trust block.
func DiffLines(a, b []byte) []string {
	split := func(x []byte) []string {
		var l []string
		for _, s := range strings.Split(string(bytes.TrimRight(x, "\n")), "\n") {
			if s != "" {
				l = append(l, s)
			}
		}
		return l
	}
	la, lb := split(a), split(b)
	count := map[string]int{}
	for _, s := range la {
		count[s]++
	}
	var out []string
	for _, s := range lb {
		if count[s] > 0 {
			count[s]--
			continue
		}
		out = append(out, "+ "+s)
	}
	count = map[string]int{}
	for _, s := range lb {
		count[s]++
	}
	for _, s := range la {
		if count[s] > 0 {
			count[s]--
			continue
		}
		out = append(out, "- "+s)
	}
	return out
}
