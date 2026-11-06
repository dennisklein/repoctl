//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/testkit"
)

// TestUIStartStop starts repoctl ui --no-browser in the set of the story,
// follows the launch URL as a browser would, and stops the server with
// repoctl ui --stop.
func TestUIStartStop(t *testing.T) {
	s := testkit.NewStory(t)
	cmd := exec.Command(filepath.Join(s.Bin, "repoctl"), "ui", "--no-browser")
	cmd.Dir = s.SetDir
	cmd.Env = s.Env()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		lines <- line
		_, _ = io.Copy(io.Discard, stdout)
		exited <- cmd.Wait()
	}()
	var link string
	select {
	case line := <-lines:
		link = strings.TrimSpace(line)
	case <-time.After(30 * time.Second):
		t.Fatalf("repoctl ui printed no URL\nstderr:\n%s", stderr.String())
	}
	if !strings.HasPrefix(link, "http://127.0.0.1:") || !strings.Contains(link, "/-/auth?t=") {
		t.Fatalf("launch URL: %q\nstderr:\n%s", link, stderr.String())
	}

	// The browser follows the link, gets the session cookie and lands on
	// the overview of the set.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	resp, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), testkit.SetName) {
		t.Fatalf("GET the launch URL: %d\n%s", resp.StatusCode, body)
	}
	// Without the cookie, a page asks for a new session.
	bare := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base, _, _ := strings.Cut(link, "/-/auth")
	resp, err = bare.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("GET / without a session: %d, want 303", resp.StatusCode)
	}

	// A second start finds the server and prints a new link.
	again := s.MustRun("ui", "--no-browser")
	if !strings.Contains(again.Stdout, "/-/auth?t=") || !strings.Contains(again.Stderr, "the server runs already") {
		t.Errorf("second start:\nstdout:\n%s\nstderr:\n%s", again.Stdout, again.Stderr)
	}

	r := s.MustRun("ui", "--stop")
	if !strings.Contains(r.Stderr, "stopped the UI server") {
		t.Errorf("ui --stop: %q", r.Stderr)
	}
	select {
	case err := <-exited:
		stopped = true
		if err != nil {
			t.Errorf("the server ended with %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server did not stop")
	}
	// Without a server, --stop has nothing to do and still succeeds.
	s.MustRun("ui", "--stop")
}

// lockedBuffer is a bytes.Buffer that the copy of a child's stderr writes
// while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
