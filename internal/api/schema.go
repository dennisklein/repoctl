package api

import (
	"embed"
	"encoding/json"
	"sort"
)

// Files holds the schema files of the contract, as schemagen copied them.
//
//go:embed schema/*.json
var Files embed.FS

// SchemaFile gives the bytes of one schema file: output.schema.json,
// events.schema.json or mcp-tools.json.
func SchemaFile(name string) []byte {
	b, err := Files.ReadFile("schema/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// OutputSchemaID and EventsSchemaID are the $id values of the two schemas.
const (
	OutputSchemaID = "https://gsi-hpc.github.io/repoctl/schema/v1alpha1/output.json"
	EventsSchemaID = "https://gsi-hpc.github.io/repoctl/schema/v1alpha1/events.json"
)

// KindSchema gives a JSON Schema of one kind or $defs entry of the output
// schema: a document that refers to the entry in the output schema.
func KindSchema(name string) ([]byte, bool) {
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(SchemaFile("output.schema.json"), &doc); err != nil {
		panic(err)
	}
	if _, ok := doc.Defs[name]; !ok {
		return nil, false
	}
	out := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$ref":    OutputSchemaID + "#/$defs/" + name,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return append(b, '\n'), true
}

// KindNames gives the names of the kinds, sorted.
func KindNames() []string {
	var out []string
	for _, k := range KindValues {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}
