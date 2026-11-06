package ui

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

func TestLaunchAttachStop(t *testing.T) {
	root := t.TempDir()
	vars := map[string]string{"HOME": filepath.Join(root, "home"), "XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_RUNTIME_DIR": filepath.Join(root, "run"), "USER": "tester"}
	if err := os.MkdirAll(vars["XDG_RUNTIME_DIR"], 0o700); err != nil {
		t.Fatal(err)
	}
	paths := config.NewPaths(func(k string) string { return vars[k] })
	setDir := filepath.Join(root, "set")
	mkfile(t, filepath.Join(setDir, "repoctl.yaml"), setYAML)
	pr, pw := io.Pipe()
	o := LaunchOptions{
		Paths: paths, Version: "1.0.0", Listen: "127.0.0.1:0", SetName: testSet, SetDir: setDir,
		Stdout: pw, Stderr: io.Discard, Getenv: func(k string) string { return vars[k] },
		Server: func() (*Server, error) {
			return New(Options{Paths: paths, Source: &fakeSource{}, Version: "1.0.0", User: "tester", Executable: "/bin/false"})
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Launch(ctx, o) }()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	link := strings.TrimSpace(line)
	if !strings.HasPrefix(link, "http://127.0.0.1:") || !strings.Contains(link, "/-/auth?t=") {
		t.Fatalf("launch URL: %q", link)
	}
	rec, running := ReadRecord(paths.UIJSON())
	if !running || rec.PID != os.Getpid() || rec.Version != "1.0.0" || rec.Port == 0 {
		t.Fatalf("ui.json: %+v %v", rec, running)
	}

	// The browser follows the link and lands on the overview of the set.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), testSet) {
		t.Fatalf("GET the launch URL: %d", resp.StatusCode)
	}
	if resp, err := client.Get(link); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("the launch URL a second time: %d, want 403", resp.StatusCode)
		}
	}

	// A second start hands its set to the server.
	var out, errOut bytes.Buffer
	second := o
	second.Stdout, second.Stderr, second.SetName = &out, &errOut, "other-set"
	if err := Launch(ctx, second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/-/auth?t=") || !strings.Contains(errOut.String(), "the server runs already") || !strings.Contains(errOut.String(), "other-set") {
		t.Errorf("second start: %q %q", out.String(), errOut.String())
	}
	// Another version: no terminal, so exit 9.
	third := second
	third.Version = "2.0.0"
	err = Launch(ctx, third)
	if e, ok := api.AsError(err); !ok || e.Code != api.PHostStaleServer || e.ExitCode != api.ExitHostNotReady {
		t.Errorf("another version: %v, want host.stale-server with exit 9", err)
	}
	if err := Remove(second, "other-set"); err != nil {
		t.Errorf("remove: %v", err)
	}
	if err := Remove(second, "other-set"); err == nil {
		t.Error("removing a set twice gives no error")
	}

	// --stop stops the server; the first Launch returns.
	errOut.Reset()
	if err := Stop(second); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the server ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
	if !strings.Contains(errOut.String(), "stopped the UI server") {
		t.Errorf("stop: %q", errOut.String())
	}
	if _, running := ReadRecord(paths.UIJSON()); running {
		t.Error("ui.json is still held")
	}
	if err := Stop(second); err != nil {
		t.Errorf("stop without a server: %v", err)
	}
	// Without a server, --remove edits the set list itself.
	if err := Remove(second, testSet); err != nil {
		t.Errorf("remove without a server: %v", err)
	}
	// A listen address that is not loopback is a usage error.
	bad := second
	bad.Listen = "0.0.0.0:0"
	if e, ok := api.AsError(Launch(ctx, bad)); !ok || e.ExitCode != api.ExitUsage {
		t.Errorf("0.0.0.0: %v, want exit 2", e)
	}
}
