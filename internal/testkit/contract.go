// Package testkit holds the helpers of the tests: the contract validator, the
// story fixture, the mock stub and the golden files
// (docs/design/delivery.html#tests).
package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/dennisklein/repoctl/internal/api"
)

var (
	compileOnce sync.Once
	compiler    *jsonschema.Compiler
	compileErr  error
	schemas     sync.Map // url -> *jsonschema.Schema
)

func load() (*jsonschema.Compiler, error) {
	compileOnce.Do(func() {
		c := jsonschema.NewCompiler()
		for id, file := range map[string]string{
			api.OutputSchemaID: "output.schema.json",
			api.EventsSchemaID: "events.schema.json",
		} {
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(api.SchemaFile(file)))
			if err != nil {
				compileErr = err
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				compileErr = err
				return
			}
		}
		compiler = c
	})
	return compiler, compileErr
}

func schema(url string) (*jsonschema.Schema, error) {
	if s, ok := schemas.Load(url); ok {
		return s.(*jsonschema.Schema), nil
	}
	c, err := load()
	if err != nil {
		return nil, err
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, err
	}
	schemas.Store(url, s)
	return s, nil
}

// Validate checks data against a schema of the contract. ref is a $defs name
// of the output schema ("Envelope", "Plan", …), "events" for one journal
// line, or "output" for the root of the output schema.
func Validate(ref string, data []byte) error {
	url := api.OutputSchemaID + "#/$defs/" + ref
	switch ref {
	case "output":
		url = api.OutputSchemaID
	case "events":
		url = api.EventsSchemaID
	}
	s, err := schema(url)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return s.Validate(inst)
}

// ValidateEnvelope fails the test when data is not one valid envelope.
func ValidateEnvelope(t testing.TB, data []byte) {
	t.Helper()
	if err := Validate("Envelope", data); err != nil {
		t.Fatalf("envelope does not agree with output.schema.json: %v\n%s", err, data)
	}
}

// ValidateJournal fails the test when a line of the journal is not valid
// against events.schema.json.
func ValidateJournal(t testing.TB, data []byte) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		if err := Validate("events", []byte(line)); err != nil {
			t.Fatalf("journal line %d does not agree with events.schema.json: %v\n%s", i+1, err, line)
		}
	}
}

// MarshalItem encodes v as JSON, for Validate of one item.
func MarshalItem(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
