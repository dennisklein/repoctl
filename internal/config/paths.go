package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Paths are the XDG directories of repoctl (config.html#xdg).
type Paths struct {
	ConfigHome string // $XDG_CONFIG_HOME/repoctl
	CacheHome  string // $XDG_CACHE_HOME/repoctl
	DataHome   string // $XDG_DATA_HOME/repoctl
	StateHome  string // $XDG_STATE_HOME/repoctl
	// RuntimeDir is $XDG_RUNTIME_DIR/repoctl, or "" without XDG_RUNTIME_DIR.
	RuntimeDir string
	// MockDir is the directory of the mock roots, /var/lib/mock.
	MockDir string
	// MockConfigDir is the directory of the host mock configs, /etc/mock.
	MockConfigDir string
	// TestHost relaxes the host checks that a test host cannot pass: the
	// group mock, podman, the rpmautospec plugin and the free disk.
	TestHost bool
}

// Test hooks: the test kit runs repoctl against a fake mock in temporary
// directories. They are not part of the documented environment.
const (
	EnvTestMockDir       = "REPOCTL_TEST_MOCK_DIR"
	EnvTestMockConfigDir = "REPOCTL_TEST_MOCK_CONFIG_DIR"
	EnvTestHost          = "REPOCTL_TEST_HOST"
)

// NewPaths gives the paths from the environment.
func NewPaths(getenv func(string) string) Paths {
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	xdg := func(name, def string) string {
		if v := getenv(name); v != "" && filepath.IsAbs(v) {
			return filepath.Join(v, "repoctl")
		}
		return filepath.Join(home, def, "repoctl")
	}
	p := Paths{
		ConfigHome: xdg("XDG_CONFIG_HOME", ".config"),
		CacheHome:  xdg("XDG_CACHE_HOME", ".cache"),
		DataHome:   xdg("XDG_DATA_HOME", filepath.Join(".local", "share")),
		StateHome:  xdg("XDG_STATE_HOME", filepath.Join(".local", "state")),
	}
	if v := getenv("XDG_RUNTIME_DIR"); v != "" && filepath.IsAbs(v) {
		p.RuntimeDir = filepath.Join(v, "repoctl")
	}
	p.MockDir, p.MockConfigDir = "/var/lib/mock", "/etc/mock"
	if v := getenv(EnvTestMockDir); v != "" {
		p.MockDir = v
	}
	if v := getenv(EnvTestMockConfigDir); v != "" {
		p.MockConfigDir = v
	}
	p.TestHost = getenv(EnvTestHost) == "1"
	return p
}

// UserConfig gives the default path of the user config.
func (p Paths) UserConfig() string { return filepath.Join(p.ConfigHome, "config.yaml") }

// SRPMCache is the SRPM cache of all sets.
func (p Paths) SRPMCache() string { return filepath.Join(p.CacheHome, "srpm") }

// SourceCache is the source cache of all sets, by SHA-512.
func (p Paths) SourceCache() string { return filepath.Join(p.CacheHome, "sources") }

// UpstreamCache is the cache of the upstream answers.
func (p Paths) UpstreamCache() string { return filepath.Join(p.CacheHome, "upstream") }

// MockWork is the work directory of a run: wrapper configs, macro files,
// source clones and result directories.
func (p Paths) MockWork(set, run string) string {
	return filepath.Join(p.CacheHome, "mock", set, run)
}

// SetData is $XDG_DATA_HOME/repoctl/SET.
func (p Paths) SetData(set string) string { return filepath.Join(p.DataHome, set) }

// Staging is the staging of a run: SET/staging/RUN.
func (p Paths) Staging(set, run string) string {
	return filepath.Join(p.SetData(set), "staging", run)
}

// StagingTarget is the staging of a run for one target.
func (p Paths) StagingTarget(set, run, target string) string {
	return filepath.Join(p.Staging(set, run), target)
}

// LocalCopy is the local copy of latest of a target: SET/repo/TARGET.
func (p Paths) LocalCopy(set, target string) string {
	return filepath.Join(p.SetData(set), "repo", target)
}

// Publish is the tree and the state file of a publish: SET/publishes/ID.
func (p Paths) Publish(set, id string) string {
	return filepath.Join(p.SetData(set), "publishes", id)
}

// GitClones is the directory of the bare clones of external packages.
func (p Paths) GitClones() string { return filepath.Join(p.DataHome, "git") }

// SetState is $XDG_STATE_HOME/repoctl/SET.
func (p Paths) SetState(set string) string { return filepath.Join(p.StateHome, set) }

// Runs is the directory of the journals of a set.
func (p Paths) Runs(set string) string { return filepath.Join(p.SetState(set), "runs") }

// Journal is the journal of a run.
func (p Paths) Journal(set, run string) string {
	return filepath.Join(p.Runs(set), run+".jsonl")
}

// ProgressLog is the go-clikit event log of a run.
func (p Paths) ProgressLog(set, run string) string {
	return filepath.Join(p.Runs(set), run+".progress.jsonl")
}

// Logs is the log directory of a run.
func (p Paths) Logs(set, run string) string { return filepath.Join(p.SetState(set), "logs", run) }

// NodeLog is the log of one node in a run.
func (p Paths) NodeLog(set, run, node string) string {
	return filepath.Join(p.Logs(set, run), node+".log")
}

// MockLogs is the directory of the mock logs of one step of a node.
func (p Paths) MockLogs(set, run, node, step string) string {
	return filepath.Join(p.Logs(set, run), node, step)
}

// CheckJSON is the result of the last repoctl check.
func (p Paths) CheckJSON(set string) string { return filepath.Join(p.SetState(set), "check.json") }

// Agent is the directory of the agent results of a set.
func (p Paths) Agent(set string) string { return filepath.Join(p.SetState(set), "agent") }

// Activity is the go-clikit event log of the commands without a run.
func (p Paths) Activity() string { return filepath.Join(p.StateHome, "activity.jsonl") }

// UISets is the set list of the web UI.
func (p Paths) UISets() string { return filepath.Join(p.StateHome, "-ui", "sets.yaml") }

// Runtime is the runtime directory, or the state directory without
// XDG_RUNTIME_DIR (after su, in cron or in CI).
func (p Paths) Runtime() string {
	if p.RuntimeDir != "" {
		return p.RuntimeDir
	}
	return p.StateHome
}

// SetLock is the set lock file of a set.
func (p Paths) SetLock(set string) string { return filepath.Join(p.Runtime(), set+".lock") }

// CheckLock is the check lock file of a set.
func (p Paths) CheckLock(set string) string { return filepath.Join(p.Runtime(), set+".check.lock") }

// CloneLock is the clone lock of the bare clone with the hash.
func (p Paths) CloneLock(hash string) string {
	return filepath.Join(p.Runtime(), "git-"+hash+".lock")
}

// UIJSON is the file that names the running UI server.
func (p Paths) UIJSON() string { return filepath.Join(p.Runtime(), "ui.json") }

// UISocket is the control socket of the UI server.
func (p Paths) UISocket() string { return filepath.Join(p.Runtime(), "ui.sock") }

// UIChildren is the directory of the output of the UI children without a run.
func (p Paths) UIChildren() string { return filepath.Join(p.Runtime(), "ui") }

// HostMockLock gives the host mock lock of all users and sets,
// /var/lib/mock/.repoctl.lock.
func (p Paths) HostMockLock() string { return filepath.Join(p.MockDir, ".repoctl.lock") }

// MockConfig gives the host file of a mock config name.
func (p Paths) MockConfig(name string) string { return filepath.Join(p.MockConfigDir, name+".cfg") }

// MockRootPrefix gives the --uniqueext prefix of the roots of this user.
func MockRootPrefix() string { return "repoctl-" + strconv.Itoa(os.Getuid()) }
