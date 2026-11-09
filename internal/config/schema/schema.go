// Package schema holds the JSON Schemas (draft 2020-12) of the files that
// repoctl reads: repoctl.yaml, repoctl.lock, the user config and the
// RepoManifest. repoctl
// schema config|lock|user-config|manifest prints them for editors and agents
// (contract.html#schemas, config.html).
package schema

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed *.schema.json
var files embed.FS

// byName maps the name of repoctl schema to the file.
var byName = map[string]string{
	"config":      "config.schema.json",
	"lock":        "lock.schema.json",
	"manifest":    "manifest.schema.json",
	"user-config": "user-config.schema.json",
}

// Names gives the names of the file schemas, sorted.
func Names() []string {
	out := make([]string, 0, len(byName))
	for n := range byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// File gives the schema of a name, or nil for an unknown name.
func File(name string) []byte {
	f, ok := byName[name]
	if !ok {
		return nil
	}
	b, err := files.ReadFile(f)
	if err != nil {
		return nil
	}
	return b
}

// ID gives the $id of the schema of a name.
func ID(name string) string {
	return "https://gsi-hpc.github.io/repoctl/schema/v1alpha1/" + name + ".json"
}

// compiled holds the compiled schemas, by name.
var (
	compileMu sync.Mutex
	compiled  = map[string]*jsonschema.Schema{}
)

// Validate checks a JSON document against the schema of a name. The error
// names the first field that does not agree, for example
// "at /builds/0: missing property 'commit'".
func Validate(name string, data []byte) error {
	sch, err := compiledSchema(name)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(firstCause(ve))
		}
		return err
	}
	return nil
}

func compiledSchema(name string) (*jsonschema.Schema, error) {
	compileMu.Lock()
	defer compileMu.Unlock()
	if s, ok := compiled[name]; ok {
		return s, nil
	}
	b := File(name)
	if b == nil {
		return nil, fmt.Errorf("no schema %q", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(ID(name), doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(ID(name))
	if err != nil {
		return nil, err
	}
	compiled[name] = s
	return s, nil
}

// firstCause gives the innermost first error of a validation error, with
// the location of the field.
func firstCause(ve *jsonschema.ValidationError) string {
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	loc := "/" + strings.Join(ve.InstanceLocation, "/")
	return fmt.Sprintf("at %s: %v", loc, ve.ErrorKind)
}
