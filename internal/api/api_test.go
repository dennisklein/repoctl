package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/testkit"
)

const docs = "../../docs/design/contract"

// TestSchemaCopies checks that the embedded schema files equal the files of
// the design record.
func TestSchemaCopies(t *testing.T) {
	for _, f := range []string{"output.schema.json", "events.schema.json", "mcp-tools.json"} {
		want, err := os.ReadFile(filepath.Join(docs, "schema", f))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, api.SchemaFile(f)) {
			t.Errorf("schema/%s differs from docs: run go generate ./internal/api", f)
		}
	}
}

// TestGenerated runs schemagen again and compares its output.
func TestGenerated(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	dir := t.TempDir()
	cmd := exec.Command("go", "run", "./internal/schemagen", "-src", filepath.Join(docs, "schema"), "-dst", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("schemagen: %v\n%s", err, out)
	}
	for _, f := range []string{"zz_output.go", "zz_events.go"} {
		got, _ := os.ReadFile(filepath.Join(dir, f))
		have, _ := os.ReadFile(f)
		if !bytes.Equal(got, have) {
			t.Errorf("%s is stale: run go generate ./internal/api", f)
		}
	}
}

// TestExamples validates every example file of the contract.
func TestExamples(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(docs, "examples", "*"))
	if len(files) == 0 {
		t.Fatal("no examples")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasSuffix(f, ".jsonl"):
			testkit.ValidateJournal(t, b)
		case strings.HasPrefix(filepath.Base(f), "mcp-"):
			// MCP messages: checked by internal/mcp.
		default:
			testkit.ValidateEnvelope(t, b)
		}
	}
}

// TestProblemRegistry compares the problem codes with the table of the
// contract chapter.
func TestProblemRegistry(t *testing.T) {
	b, err := os.ReadFile("../../docs/design/contract.html")
	if err != nil {
		t.Fatal(err)
	}
	sec := string(b)
	sec = sec[strings.Index(sec, `id="problems"`):strings.Index(sec, `id="journal"`)]
	re := regexp.MustCompile(`<tr[^>]*>\s*<td[^>]*>(?:<[^>]+>)*<code>([a-z0-9.-]+)</code>`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(sec, -1) {
		seen[m[1]] = true
		if _, ok := api.Problems[m[1]]; !ok {
			t.Errorf("code %s of the contract is not in api.Problems", m[1])
		}
	}
	for code := range api.Problems {
		if !seen[code] {
			t.Errorf("code %s of api.Problems is not in the contract", code)
		}
	}
	// The codes outside the table are in Unlisted, with an exit code, a
	// meaning and a hint, and never in the table.
	for code, info := range api.Unlisted {
		if seen[code] {
			t.Errorf("code %s of api.Unlisted is in the contract", code)
		}
		if info.ExitText == "" || info.Meaning == "" || info.Hint == "" {
			t.Errorf("code %s of api.Unlisted: %+v", code, info)
		}
		if got, ok := api.Lookup(code); !ok || got != info {
			t.Errorf("Lookup(%s) = %+v, %v", code, got, ok)
		}
	}
	if e := api.Errorf(api.PMigrateNotes, "x"); e.ExitCode != api.ExitFailed {
		t.Errorf("a warning code gives exit %d", e.ExitCode)
	}
	if e := api.Errorf(api.PUpstreamRelease, "x"); e.ExitCode != 11 {
		t.Errorf("upstream.release gives exit %d", e.ExitCode)
	}
}

// TestRoundTrip decodes every example envelope into the Go types, with no
// unknown field, encodes it again and validates the result.
func TestRoundTrip(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(docs, "examples", "*.json"))
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "mcp-") {
			continue
		}
		b, _ := os.ReadFile(f)
		var env struct {
			api.Envelope
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out := env.Envelope
		out.Items = nil
		for i, raw := range env.Items {
			item := api.NewItem(env.Kind)
			if item == nil {
				t.Fatalf("%s: no type for kind %s", f, env.Kind)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(item); err != nil {
				t.Errorf("%s: item %d: %v", filepath.Base(f), i, err)
				continue
			}
			out.Items = append(out.Items, item)
		}
		again, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		testkit.ValidateEnvelope(t, again)
	}
}

// TestJournalRoundTrip decodes each line of the journal example into the
// struct of its type, with no unknown field.
func TestJournalRoundTrip(t *testing.T) {
	b, _ := os.ReadFile(filepath.Join(docs, "examples", "journal.jsonl"))
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var h api.EventHeader
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			t.Fatal(err)
		}
		mk, ok := api.EventStructs[h.Type]
		if !ok {
			t.Errorf("line %d: unknown type %s", i+1, h.Type)
			continue
		}
		v := mk()
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("line %d (%s): %v", i+1, h.Type, err)
			continue
		}
		again, _ := json.Marshal(v)
		if err := testkit.Validate("events", again); err != nil {
			t.Errorf("line %d (%s) again: %v\n%s", i+1, h.Type, err, again)
		}
	}
}
