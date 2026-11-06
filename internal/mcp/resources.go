package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dennisklein/repoctl/internal/api"
)

// maxNodeLog is the tail of a node log that repoctl://runs/{run}/logs/{node}
// gives.
const maxNodeLog = 256 << 10

// The URI prefix of the resources.
const uriPrefix = "repoctl://"

// addResources adds the resources and resource templates of the catalog.
func (s *Server) addResources(srv *sdk.Server) {
	for _, r := range s.cat.Resources {
		srv.AddResource(&sdk.Resource{URI: r.URI, Name: r.Name, MIMEType: r.MIMEType}, s.readResource)
	}
	for _, r := range s.cat.ResourceTemplates {
		srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: r.URITemplate, Name: r.Name, MIMEType: r.MIMEType}, s.readResource)
	}
}

// readResource reads one resource.
func (s *Server) readResource(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	uri := req.Params.URI
	text, mime, err := s.resource(ctx, uri)
	if err != nil {
		return nil, err
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: mime, Text: string(text)}}}, nil
}

// resource gives the content of a resource and its MIME type.
func (s *Server) resource(ctx context.Context, uri string) ([]byte, string, error) {
	rest, ok := strings.CutPrefix(uri, uriPrefix)
	if !ok {
		return nil, "", sdk.ResourceNotFoundError(uri)
	}
	parts := strings.Split(rest, "/")
	switch {
	case rest == "schema/output":
		return api.SchemaFile("output.schema.json"), "application/schema+json", nil
	case rest == "schema/events":
		return api.SchemaFile("events.schema.json"), "application/schema+json", nil
	case rest == "schema/config":
		out, errb, exit := s.execute(ctx, nil, []string{"schema", "config"}, false)
		if exit != 0 {
			return nil, "", fmt.Errorf("repoctl schema config: exit %d: %s", exit, strings.TrimSpace(string(errb)))
		}
		return out, "application/schema+json", nil
	case rest == "status":
		return s.envelopeResource(ctx, "repoctl_status", []string{"status"}), "application/json", nil
	case len(parts) == 2 && parts[0] == "runs":
		if !reRun.MatchString(parts[1]) {
			return nil, "", sdk.ResourceNotFoundError(uri)
		}
		return s.envelopeResource(ctx, "repoctl_run_get", []string{"runs", "--", parts[1]}), "application/json", nil
	case len(parts) == 3 && parts[0] == "runs" && parts[2] == "journal":
		if !reRun.MatchString(parts[1]) {
			return nil, "", sdk.ResourceNotFoundError(uri)
		}
		b, err := os.ReadFile(s.opts.Paths.Journal(s.opts.SetName, parts[1]))
		if err != nil {
			return nil, "", sdk.ResourceNotFoundError(uri)
		}
		return b, "application/x-ndjson", nil
	case len(parts) == 4 && parts[0] == "runs" && parts[2] == "logs":
		if !reRun.MatchString(parts[1]) || !reNode.MatchString(parts[3]) {
			return nil, "", sdk.ResourceNotFoundError(uri)
		}
		b, err := tailFile(s.opts.Paths.NodeLog(s.opts.SetName, parts[1], parts[3]), maxNodeLog)
		if err != nil {
			return nil, "", sdk.ResourceNotFoundError(uri)
		}
		return b, "text/plain", nil
	case len(parts) == 2 && parts[0] == "skills":
		if s.opts.Skill == nil {
			return nil, "", sdk.ResourceNotFoundError(uri)
		}
		b, ok := s.opts.Skill(parts[1])
		if !ok {
			return nil, "", sdk.ResourceNotFoundError(uri)
		}
		return b, "text/markdown", nil
	}
	return nil, "", sdk.ResourceNotFoundError(uri)
}

// envelopeResource gives the envelope of a command as a resource.
func (s *Server) envelopeResource(ctx context.Context, tool string, argv []string) []byte {
	t, _ := s.cat.Tool(tool)
	if err := s.checkCommand(argv); err != nil {
		e, ok := api.AsError(err)
		p := api.Problem{Code: api.PInternal, Message: err.Error()}
		if ok {
			p = e.Problem
		}
		return s.problemEnvelope(t.OutputKind, strings.Join(commandPath(argv), " "), p)
	}
	out, errb, exit := s.execute(ctx, nil, argv, true)
	return compactJSON(s.envelopeOf(t, argv, out, errb, exit))
}

// tailFile gives the last max bytes of a file, from the start of a line.
func tailFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() <= maxBytes {
		return io.ReadAll(f)
	}
	if _, err := f.Seek(fi.Size()-maxBytes, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if i := strings.IndexByte(string(b), '\n'); i >= 0 && i+1 < len(b) {
		b = b[i+1:]
	}
	return b, nil
}

// subscribe records a subscription; only the resources with subscribe in
// the catalog take one.
func (s *Server) subscribe(_ context.Context, req *sdk.SubscribeRequest) error {
	uri := req.Params.URI
	if !s.subscribable(uri) {
		return fmt.Errorf("repoctl: resource %s takes no subscription", uri)
	}
	s.subMu.Lock()
	s.subs[uri] = true
	s.subMu.Unlock()
	return nil
}

func (s *Server) unsubscribe(_ context.Context, req *sdk.UnsubscribeRequest) error {
	s.subMu.Lock()
	delete(s.subs, req.Params.URI)
	s.subMu.Unlock()
	return nil
}

// subscribable reports repoctl://status and repoctl://runs/{run}.
func (s *Server) subscribable(uri string) bool {
	rest, ok := strings.CutPrefix(uri, uriPrefix)
	if !ok {
		return false
	}
	if rest == "status" {
		return true
	}
	run, ok := strings.CutPrefix(rest, "runs/")
	return ok && reRun.MatchString(run)
}

// watch sends resources/updated for the subscribed resources when their
// files change: the journals and check.json of the set for
// repoctl://status, the journal of the run for repoctl://runs/{run}.
func (s *Server) watch(ctx context.Context) {
	last := map[string]string{}
	t := time.NewTicker(s.opts.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.subMu.Lock()
		uris := make([]string, 0, len(s.subs))
		for u := range s.subs {
			uris = append(uris, u)
		}
		s.subMu.Unlock()
		for _, u := range uris {
			fp := s.fingerprint(u)
			old, seen := last[u]
			last[u] = fp
			if seen && old != fp && s.srv != nil {
				_ = s.srv.ResourceUpdated(ctx, &sdk.ResourceUpdatedNotificationParams{URI: u})
			}
		}
		for u := range last {
			if !contains(uris, u) {
				delete(last, u)
			}
		}
	}
}

// fingerprint gives a text that changes when a subscribed resource changes.
func (s *Server) fingerprint(uri string) string {
	rest := strings.TrimPrefix(uri, uriPrefix)
	if run, ok := strings.CutPrefix(rest, "runs/"); ok {
		return statText(s.opts.Paths.Journal(s.opts.SetName, run))
	}
	var b strings.Builder
	b.WriteString(statText(s.opts.Paths.CheckJSON(s.opts.SetName)))
	entries, _ := os.ReadDir(s.opts.Paths.Runs(s.opts.SetName))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") && !strings.HasSuffix(e.Name(), ".progress.jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString(";" + n + "=" + statText(s.opts.Paths.Runs(s.opts.SetName)+"/"+n))
	}
	return b.String()
}

func statText(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d@%d", fi.Size(), fi.ModTime().UnixNano())
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
