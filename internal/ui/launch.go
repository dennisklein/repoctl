package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb"
	"github.com/GSI-HPC/go-clikit/localweb/launch"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// LaunchOptions configures Launch, Stop and Remove: what `repoctl ui`
// takes from its command line, its settings and its process.
type LaunchOptions struct {
	Paths   config.Paths
	Version string
	// Listen is the setting ui.listen.
	Listen string
	// Open opens the browser (ui.open, and no --no-browser).
	Open bool
	// SetName and SetDir are the set of the working directory, or "".
	SetName, SetDir string
	Stdout, Stderr  io.Writer
	Getenv          func(string) string
	// Ask asks a yes or no question on the terminal; nil means there is
	// no terminal.
	Ask func(question string) bool
	// Server makes the server when this process becomes it.
	Server func() (*Server, error)
	// OpenURL opens the browser; nil is launch.Opener with xdg-open.
	OpenURL func(url string) error
	// Wait is how long a second start retries the server that holds
	// ui.json (default 5 s).
	Wait time.Duration
}

func (o *LaunchOptions) note(format string, args ...any) {
	fmt.Fprintf(o.Stderr, "note: "+format+"\n", args...)
}

// setPath gives the overview of a set, or the page of all sets.
func setPath(set string) string {
	if set == "" {
		return "/"
	}
	return "/s/" + url.PathEscape(set) + "/"
}

// Launch is `repoctl ui`: it hands the set to the server of the user when
// one runs and opens the browser; else this process becomes the server
// and serves until ctx ends. Its errors carry the exit code: 1 when it
// cannot listen, 2 for an address that is not loopback, 9 when the server
// runs another version.
func Launch(ctx context.Context, o LaunchOptions) error {
	if o.Wait == 0 {
		o.Wait = 5 * time.Second
	}
	if err := localweb.CheckAddr(o.Listen); err != nil {
		return api.Errorf(api.PConfigInvalid, "ui.listen: %v", err).WithExit(api.ExitUsage).
			WithHint("use a loopback address such as 127.0.0.1:0 or localhost:8080", "")
	}
	deadline := time.Now().Add(o.Wait)
	for {
		if ans, err := Control(o.Paths.UISocket(), ControlRequest{Op: OpHello}); err == nil {
			if ans.Version != o.Version {
				q := fmt.Sprintf("The UI server runs repoctl %s. Restart it with %s? [y/N] ", ans.Version, o.Version)
				if o.Ask == nil || !o.Ask(q) {
					return api.Errorf(api.PHostStaleServer, "the UI server (pid %d) runs repoctl %s, not %s", ans.PID, ans.Version, o.Version).
						WithHint("stop it and start it again", "repoctl ui --stop && repoctl ui")
				}
				if err := stopServer(o, false); err != nil {
					return err
				}
				continue
			}
			return attach(o, ans)
		}
		rec, err := lockRecord(o.Paths.UIJSON())
		if err == nil {
			return serve(ctx, o, rec)
		}
		if !errors.Is(err, errHeld) {
			return api.Errorf(api.PInternal, "ui.json: %v", err)
		}
		// Another server starts, or one hangs.
		if time.Now().After(deadline) {
			r, _ := ReadRecord(o.Paths.UIJSON())
			return api.Errorf(api.PInternal, "a UI server (pid %d) holds %s but does not answer on %s", r.PID, o.Paths.UIJSON(), o.Paths.UISocket()).
				WithHint("stop it", "repoctl ui --stop")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// attach hands the set to the running server and opens its URL.
func attach(o LaunchOptions, hello ControlAnswer) error {
	req := ControlRequest{Op: OpToken, Next: setPath(o.SetName)}
	if o.SetName != "" {
		req = ControlRequest{Op: OpAddSet, Set: o.SetName, Path: o.SetDir}
	}
	ans, err := Control(o.Paths.UISocket(), req)
	if err != nil {
		return api.Errorf(api.PInternal, "the UI server did not answer: %v", err)
	}
	fmt.Fprintln(o.Stdout, ans.URL)
	since := hello.Started
	if t, err := api.ParseTime(hello.Started); err == nil {
		since = t.Local().Format("15:04")
	}
	msg := fmt.Sprintf("the server runs already (pid %d, since %s).", hello.PID, since)
	if o.SetName != "" {
		msg += " It now also serves " + o.SetName + "."
	}
	o.note("%s", msg)
	openBrowser(o, ans.URL)
	return nil
}

// serve makes this process the server, until ctx ends or a stop request.
func serve(ctx context.Context, o LaunchOptions, rec *recordFile) error {
	defer rec.release()
	bound, err := localweb.Listen(ctx, o.Listen, localweb.ListenOptions{AnyPort: true})
	if err != nil {
		return api.Errorf(api.PInternal, "cannot listen on %s: %v", o.Listen, err)
	}
	ls := bound.Listeners
	for i, l := range ls {
		pl, err := localweb.PeerListener(l, os.Getuid(), nil)
		if err != nil {
			_ = bound.Close()
			return api.Errorf(api.PInternal, "cannot check the peers of %s: %v", o.Listen, err)
		}
		ls[i] = pl
	}
	ctl, err := localweb.Listen(ctx, localweb.UnixPrefix+o.Paths.UISocket(), localweb.ListenOptions{})
	if err != nil {
		_ = bound.Close()
		return api.Errorf(api.PInternal, "cannot listen on %s: %v", o.Paths.UISocket(), err)
	}
	srv, err := o.Server()
	if err != nil {
		_ = bound.Close()
		_ = ctl.Close()
		return err
	}
	if o.SetName != "" {
		if _, err := srv.AddSet(o.SetName, o.SetDir); err != nil {
			o.note("cannot write the set list: %v", err)
		}
	}
	host := bound.Host()
	if h, _, _ := net.SplitHostPort(o.Listen); h == "localhost" {
		host = "localhost:" + fmt.Sprint(bound.Addrs[0].Port())
	}
	base := "http://" + host
	if err := rec.write(Record{PID: os.Getpid(), Listen: host, Port: int(bound.Addrs[0].Port()), URL: base, Version: o.Version,
		Started: api.FormatTime(srv.started), Session: srv.SessionID()}); err != nil {
		_ = bound.Close()
		_ = ctl.Close()
		return api.Errorf(api.PInternal, "cannot write %s: %v", o.Paths.UIJSON(), err)
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go srv.Watch(ctx)
	go srv.serveControl(ctx, ctl.Listeners[0], base, stop)
	link := base + srv.LaunchPath(setPath(o.SetName))
	fmt.Fprintln(o.Stdout, link)
	opened := openBrowser(o, link)
	who := os.Getenv("USER")
	if o.Getenv != nil {
		who = o.Getenv("USER")
	}
	if opened {
		o.note("opened in the browser. The server is for %s on this host only. Ctrl-C stops it.", who)
	} else {
		o.note("open the URL in a browser on this host; it is good once, for 60 s. The server is for %s on this host only. Ctrl-C stops it.", who)
	}
	err = localweb.Serve(ctx, ls, srv.Handler(bound.Answers), localweb.ServeOptions{ErrorLog: srv.log})
	srv.Stop()
	if err != nil {
		return api.Errorf(api.PInternal, "the UI server: %v", err)
	}
	return nil
}

// openBrowser opens url unless o says not to, and reports whether it did.
func openBrowser(o LaunchOptions, link string) bool {
	if !o.Open {
		return false
	}
	if o.OpenURL != nil {
		return o.OpenURL(link) == nil
	}
	op := launch.Opener{Name: "repoctl ui", GOOS: runtime.GOOS, Getenv: o.Getenv, Dir: o.Paths.UIChildren()}
	if why := op.Skip(); why != "" {
		o.note("no browser opens: %s.", why)
		return false
	}
	if err := os.MkdirAll(o.Paths.UIChildren(), 0o700); err != nil {
		return false
	}
	if _, err := op.Open(link); err != nil {
		o.note("no browser opens: %v.", err)
		return false
	}
	return true
}

// Stop is `repoctl ui --stop`: it stops the server of the user and names
// the runs that its children started, which go on.
func Stop(o LaunchOptions) error { return stopServer(o, true) }

func stopServer(o LaunchOptions, say bool) error {
	rec, running := ReadRecord(o.Paths.UIJSON())
	if !running {
		if say {
			o.note("no UI server runs.")
		}
		return nil
	}
	ans, err := Control(o.Paths.UISocket(), ControlRequest{Op: OpStop})
	if err != nil {
		if kerr := syscall.Kill(rec.PID, syscall.SIGTERM); kerr != nil {
			return api.Errorf(api.PInternal, "cannot stop the UI server (pid %d): %v", rec.PID, kerr)
		}
	}
	for i := 0; i < 100 && recordHeld(o.Paths.UIJSON()); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if recordHeld(o.Paths.UIJSON()) {
		return api.Errorf(api.PInternal, "the UI server (pid %d) did not stop within 10 s", rec.PID)
	}
	if say {
		o.note("stopped the UI server (pid %d).", rec.PID)
		for _, r := range ans.Runs {
			o.note("the run %s, started from the UI, goes on.", r)
		}
	}
	return nil
}

// Remove is `repoctl ui --remove SET`: it takes the set off the set list,
// through the server when one runs. The files of the set stay.
func Remove(o LaunchOptions, name string) error {
	removed := false
	if _, running := ReadRecord(o.Paths.UIJSON()); running {
		ans, err := Control(o.Paths.UISocket(), ControlRequest{Op: OpRemoveSet, Set: name})
		if err != nil {
			return api.Errorf(api.PInternal, "the UI server did not answer: %v", err)
		}
		removed = ans.Removed
	} else {
		var err error
		if removed, err = RemoveFromSetList(o.Paths.UISets(), name); err != nil {
			return api.Errorf(api.PInternal, "%v", err)
		}
	}
	if !removed {
		return api.Errorf(api.PConfigUnknownSet, "the set %s is not on the set list of the UI", name).WithExit(api.ExitFailed)
	}
	o.note("removed %s from the set list. Its files stay.", name)
	return nil
}

// StaleExecutable says why the binary of this process is no longer the
// one on disk: on Linux, /proc/self/exe ends in " (deleted)" after an
// upgrade replaced it. "" when it is the same.
func StaleExecutable() string {
	p, err := os.Readlink("/proc/self/exe")
	if err != nil || !strings.HasSuffix(p, " (deleted)") {
		return ""
	}
	return strings.TrimSuffix(p, " (deleted)") + " was replaced"
}

func osPID() int { return os.Getpid() }
