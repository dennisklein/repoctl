package advisory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/fsutil"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// The template pair of the check timer (advisories.html#timer). The units
// call /usr/bin/repoctl, so the timer runs the new binary after an
// upgrade.
const (
	ServiceTemplate = "repoctl-check@.service"
	TimerTemplate   = "repoctl-check@.timer"

	ServiceUnit = `[Unit]
Description=repoctl check for the set in %I
After=network-online.target

[Service]
Type=oneshot
EnvironmentFile=-%h/.config/repoctl/check.env
ExecStart=/usr/bin/repoctl -C %I check --quiet
Nice=10
`
	TimerUnit = `[Unit]
Description=Daily repoctl check for the set in %I

[Timer]
OnCalendar=*-*-* 06:00:00
RandomizedDelaySec=10min
Persistent=true

[Install]
WantedBy=timers.target
`
)

// UnitDir gives the directory of the user units:
// $XDG_CONFIG_HOME/systemd/user.
func UnitDir(p config.Paths) string {
	return filepath.Join(filepath.Dir(p.ConfigHome), "systemd", "user")
}

// EscapePath escapes a path as systemd-escape --path does.
func EscapePath(p string) string {
	p = filepath.Clean(p)
	p = strings.Trim(p, "/")
	if p == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && i == 0:
			fmt.Fprintf(&b, `\x%02x`, c)
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == ':' || c == '_' || c == '.':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// UnescapePath reverses EscapePath.
func UnescapePath(s string) string {
	if s == "-" {
		return "/"
	}
	var b strings.Builder
	b.WriteByte('/')
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '-':
			b.WriteByte('/')
		case s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x':
			var c byte
			if _, err := fmt.Sscanf(s[i+2:i+4], "%02x", &c); err == nil {
				b.WriteByte(c)
				i += 3
				continue
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// Timer is the check timer of one set directory on this host.
type Timer struct {
	UnitDir string
	SetDir  string
	// TestHost writes the files and the enable link but never calls
	// systemctl.
	TestHost bool
	// User is the login name, for the linger note.
	User string
	Now  func() time.Time
}

// Instance gives the timer instance of the set: repoctl-check@ESC.timer.
func (t *Timer) Instance() string { return "repoctl-check@" + EscapePath(t.SetDir) + ".timer" }

func (t *Timer) wantsLink() string {
	return filepath.Join(t.UnitDir, "timers.target.wants", t.Instance())
}

func systemctl(ctx context.Context, args ...string) (string, error) {
	out, err := (&run.Cmd{Name: "systemctl", Args: append([]string{"--user"}, args...)}).Output(ctx)
	return strings.TrimSpace(string(out)), err
}

// InstallResult says what Install did.
type InstallResult struct {
	Wrote   []string // template files written for the first time
	Updated []string // template files that differed from this binary
	Started string   // the instance
	Next    string   // the next run, for example "tomorrow 06:00"
	Linger  bool     // lingering is on
}

// Install writes the template pair when it differs from this binary and
// enables and starts the instance of the set.
func (t *Timer) Install(ctx context.Context) (*InstallResult, error) {
	res := &InstallResult{Started: t.Instance()}
	for _, f := range []struct{ name, text string }{{ServiceTemplate, ServiceUnit}, {TimerTemplate, TimerUnit}} {
		path := filepath.Join(t.UnitDir, f.name)
		old, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			res.Wrote = append(res.Wrote, path)
		case err != nil:
			return nil, err
		case bytes.Equal(old, []byte(f.text)):
			continue
		default:
			res.Updated = append(res.Updated, path)
		}
		if err := os.MkdirAll(t.UnitDir, 0o755); err != nil {
			return nil, err
		}
		if err := fsutil.WriteAtomic(path, []byte(f.text), 0o644); err != nil {
			return nil, err
		}
	}
	if t.TestHost {
		link := t.wantsLink()
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return nil, err
		}
		_ = os.Remove(link)
		if err := os.Symlink(filepath.Join(t.UnitDir, TimerTemplate), link); err != nil {
			return nil, err
		}
		res.Next = nextRun(t.now())
	} else {
		if len(res.Wrote)+len(res.Updated) > 0 {
			if _, err := systemctl(ctx, "daemon-reload"); err != nil {
				return nil, err
			}
		}
		if _, err := systemctl(ctx, "enable", "--now", t.Instance()); err != nil {
			return nil, err
		}
		res.Next = nextRun(t.now())
	}
	res.Linger = Linger(t.User)
	return res, nil
}

// nextRun gives the next 06:00 in the words of the terminal.
func nextRun(now time.Time) string {
	next := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, now.Location())
	if !next.After(now) {
		return "tomorrow 06:00"
	}
	return "today 06:00"
}

func (t *Timer) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Remove stops and removes the instance of the set. It reports whether an
// instance was there.
func (t *Timer) Remove(ctx context.Context) (bool, error) {
	link := t.wantsLink()
	_, err := os.Lstat(link)
	had := err == nil
	if t.TestHost {
		if had {
			return true, os.Remove(link)
		}
		return false, nil
	}
	if _, err := systemctl(ctx, "disable", "--now", t.Instance()); err != nil {
		return had, err
	}
	return had, nil
}

// State gives active, off or unknown.
func (t *Timer) State(ctx context.Context) string {
	if t.TestHost {
		if _, err := os.Lstat(t.wantsLink()); err == nil {
			return "active"
		}
		return "off"
	}
	out, err := systemctl(ctx, "is-active", t.Instance())
	switch {
	case out == "active":
		return "active"
	case out != "":
		return "off"
	case err != nil:
		return "unknown"
	}
	return "off"
}

// UnitsDiffer reports that the template pair exists and differs from the
// pair of this binary.
func (t *Timer) UnitsDiffer() bool {
	for _, f := range []struct{ name, text string }{{ServiceTemplate, ServiceUnit}, {TimerTemplate, TimerUnit}} {
		b, err := os.ReadFile(filepath.Join(t.UnitDir, f.name))
		if err == nil && !bytes.Equal(b, []byte(f.text)) {
			return true
		}
	}
	return false
}

// Orphans gives the enabled instances whose set directory is gone or holds
// no set.
func Orphans(unitDir string) []string {
	entries, err := os.ReadDir(filepath.Join(unitDir, "timers.target.wants"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		esc, ok := strings.CutPrefix(n, "repoctl-check@")
		if !ok || !strings.HasSuffix(esc, ".timer") || esc == ".timer" {
			continue
		}
		dir := UnescapePath(strings.TrimSuffix(esc, ".timer"))
		if _, err := os.Stat(filepath.Join(dir, config.SetFile)); err != nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Linger reports whether lingering is on for the user.
func Linger(user string) bool {
	if user == "" {
		return false
	}
	_, err := os.Stat(filepath.Join("/var/lib/systemd/linger", user))
	return err == nil
}
