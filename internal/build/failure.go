package build

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

var reMissing = regexp.MustCompile(`No matching package to install: '([^']+)'`)

// failure gives the error of a failed step: the class from the records of
// repoctl first, then from the host, the problem code and the exit code
// of mock (build.html#failures). It writes the failure into the node log
// and gives the short text of the tree.
func (r *runner) failure(n *node, step api.Step, err error) (*api.NodeError, string) {
	ne := &api.NodeError{Step: step, ExitCode: max(run.ExitCode(err), 0)}
	logs := r.o.Paths.MockLogs(r.o.Set.Name, r.o.RunID, n.id, string(step))
	rootLog := filepath.Join(logs, "root.log")
	buildLog := filepath.Join(logs, "build.log")
	rootText, _ := os.ReadFile(rootLog)
	var fe *factsError
	if class, msg := n.stop.cause(); class != "" {
		ne.Class, ne.Message = class, msg
	} else {
		switch ae, isAPI := api.AsError(err); {
		case errors.Is(err, syscall.ENOSPC) || mock.HostProblem(rootText):
			ne.Class = api.FailureClassHost
		case errors.As(err, &fe):
			ne.Class, ne.Message = api.FailureClassMock, fe.Error()
		case isAPI && ae.Code != "" && ae.Code != api.PNodeFailed:
			ne.Class, ne.Code, ne.Message = api.FailureClassSRPM, ae.Code, ae.Message
		case isAPI:
			ne.Class, ne.Message = api.FailureClassSRPM, ae.Message
		case step == api.StepStage:
			ne.Class = api.FailureClassStage
		case run.ExitCode(err) < 0 && step != api.StepStage:
			ne.Class = api.FailureClassMock
		default:
			ne.Class = api.FailureClass(mock.ClassOf(string(step), err))
		}
	}
	short := ""
	switch ne.Class {
	case api.FailureClassBuilddep:
		for _, m := range reMissing.FindAllStringSubmatch(string(rootText), -1) {
			if !slices.Contains(ne.Missing, m[1]) {
				ne.Missing = append(ne.Missing, m[1])
			}
		}
		if len(ne.Missing) > 0 {
			verb := "is"
			if len(ne.Missing) > 1 {
				verb = "are"
			}
			short = strings.Join(ne.Missing, ", ") + " " + verb + " needed"
			ne.Message = "Failed build dependencies: " + short
		}
		ne.Excerpt = excerpt(rootLog)
	case api.FailureClassHost:
		ne.Excerpt = excerpt(rootLog)
	case api.FailureClassTimeout, api.FailureClassCanceled, api.FailureClassInternal:
	default:
		if ex := excerpt(buildLog); ex != nil {
			ne.Excerpt = ex
		} else {
			ne.Excerpt = excerpt(rootLog)
		}
	}
	if ne.Message == "" {
		ne.Message = errMessage(ne, err, buildLog)
	}
	ne.Message = firstLine(ne.Message)
	if short == "" {
		short = ne.Message
	}
	if ne.Class != api.FailureClassCanceled {
		n.log.Marker(fmt.Sprintf("%s failed: %s", step, short))
	}
	return ne, short
}

// errMessage gives the one-line message of a failure: the last error line
// of build.log, else the error of the call.
func errMessage(ne *api.NodeError, err error, buildLog string) string {
	if ne.Class == api.FailureClassBuild || ne.Class == api.FailureClassSRPM {
		lines := readLines(buildLog)
		for i := len(lines) - 1; i >= 0; i-- {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "error: "); ok {
				return rest
			}
		}
	}
	switch ne.Class {
	case api.FailureClassBuild:
		return fmt.Sprintf("mock --rebuild failed with exit %d", ne.ExitCode)
	case api.FailureClassSRPM:
		if ne.ExitCode > 0 {
			return fmt.Sprintf("mock --buildsrpm failed with exit %d", ne.ExitCode)
		}
	case api.FailureClassHost:
		if errors.Is(err, syscall.ENOSPC) {
			return "no space left on device"
		}
		return "the host failed: " + err.Error()
	}
	return err.Error()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// excerpt gives the lines of a mock log from the line above the first
// error to the last error, at most 8 lines.
func excerpt(path string) *api.NodeErrorExcerpt {
	lines := readLines(path)
	first, last := -1, -1
	for i, l := range lines {
		if ErrorLine.MatchString(l) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}
	start := max(first-1, 0)
	end := min(last+1, start+8, len(lines))
	return &api.NodeErrorExcerpt{File: path, FirstLine: start + 1, Lines: append([]string(nil), lines[start:end]...)}
}

var reSoname = regexp.MustCompile(`^[^\s/()]+\.so(\.[0-9]+)*(\(.*\))?$`)

// sonames gives the sonames of a provides list.
func sonames(provides []string) []string {
	var out []string
	for _, p := range provides {
		if reSoname.MatchString(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// sonameCheck compares the sonames of the new RPMs with those of the
// newest published build of the node (build.html#soname). A removed
// soname that a kept package of the set requires gives the advisory
// soname.removed.
func (r *runner) sonameCheck(n *node, provides []string) *api.NodeChecks {
	m, err := manifest.ReadLocal(r.o.Paths, r.o.Set, n.target)
	if err != nil || m == nil {
		return &api.NodeChecks{Soname: "none"}
	}
	prev, ok := m.Newest(n.pkg, "")
	if !ok {
		return &api.NodeChecks{Soname: "none"}
	}
	old := sonames(prev.Provides)
	if len(old) == 0 {
		return &api.NodeChecks{Soname: "none"}
	}
	var gone []string
	for _, s := range old {
		if !slices.Contains(provides, s) {
			gone = append(gone, s)
		}
	}
	if len(gone) == 0 {
		return &api.NodeChecks{Soname: "unchanged"}
	}
	type user struct {
		Package  string   `json:"package"`
		Requires []string `json:"requires"`
	}
	var users []user
	newest := map[string]*manifest.Build{}
	for i := range m.Builds {
		b := &m.Builds[i]
		if b.Package != n.pkg {
			newest[b.Package] = b
		}
	}
	for pkg, b := range newest {
		var req []string
		for _, s := range gone {
			if slices.Contains(b.Requires, s) {
				req = append(req, s)
			}
		}
		if len(req) > 0 {
			users = append(users, user{Package: pkg, Requires: req})
		}
	}
	if len(users) > 0 {
		slices.SortFunc(users, func(a, b user) int { return strings.Compare(a.Package, b.Package) })
		facts, _ := json.Marshal(map[string]any{"node": n.id, "removed": gone, "users": users})
		sum := sha256.Sum256(facts)
		r.emit(n, nil, nil, &api.AdvisoryEvent{ID: "soname.removed/" + n.id, State: api.AdvisoryStateOpen,
			Level: api.AdvisoryLevelRequired, FactsDigest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	return &api.NodeChecks{Soname: "removed"}
}
