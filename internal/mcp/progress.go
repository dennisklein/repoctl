package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/eventlog"
	"github.com/GSI-HPC/go-clikit/progress/mcpprogress"

	"github.com/dennisklein/repoctl/internal/api"
)

// pollProgress is how often a follower reads the progress log.
const pollProgress = 250 * time.Millisecond

// follower sends the progress of a command as MCP progress notifications:
// it reads the go-clikit progress log of the command as it grows and gives
// the events of its run to an mcpprogress.Notifier.
type follower struct {
	tail *eventlog.Tail
	n    *mcpprogress.Notifier
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// follow starts a follower of the progress log at path.
func follow(path string, send func(ctx context.Context, progress, total float64, message string) error) *follower {
	n := mcpprogress.New(mcpprogress.Options{Send: send, Program: "repoctl"})
	f := &follower{
		tail: eventlog.NewTail(path, eventlog.NewForwarder(n, progress.TraceID{})),
		n:    n,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go func() {
		defer close(f.done)
		t := time.NewTicker(pollProgress)
		defer t.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-t.C:
				_, _ = f.tail.Poll()
			}
		}
	}()
	return f
}

// close reads the rest of the log, then closes the notifier, so that
// nothing is sent after the result of the call.
func (f *follower) close() {
	f.once.Do(func() {
		close(f.stop)
		<-f.done
		_, _ = f.tail.Poll()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = f.n.Close(ctx)
	})
}

// runProgress sends the progress of a run from its counts: the build nodes
// that ended, of the build nodes of the plan (agents.html#progress). It
// sends only when more nodes ended.
type runProgress struct {
	send func(ctx context.Context, progress, total float64, message string) error
	last float64
	any  bool
}

func (r *runProgress) update(ctx context.Context, run *api.Run) {
	if r == nil || r.send == nil || run == nil {
		return
	}
	done, total, msg := runLine(run)
	if total == 0 || r.any && float64(done) <= r.last {
		return
	}
	r.last, r.any = float64(done), true
	_ = r.send(ctx, float64(done), float64(total), msg)
}

// runLine gives the line of a run: "build: 4/8 done, 1 failed, 1 blocked".
func runLine(run *api.Run) (done, total int, line string) {
	c := run.Counts
	done = c.Ok + c.Failed + c.Blocked + c.Canceled
	if run.Plan != nil {
		total = run.Plan.Build
	}
	if total == 0 {
		total = c.Waiting + c.Queued + c.Running + done
	}
	parts := []string{fmt.Sprintf("%d/%d done", done, total)}
	for _, x := range []struct {
		n    int
		what string
	}{{c.Failed, "failed"}, {c.Blocked, "blocked"}, {c.Canceled, "canceled"}} {
		if x.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", x.n, x.what))
		}
	}
	kind := run.Kind
	if kind == "" {
		kind = "build"
	}
	return done, total, kind + ": " + strings.Join(parts, ", ")
}
