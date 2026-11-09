// Package mcp is the MCP server of repoctl: repoctl mcp start serves the
// tools of mcp-tools.json on stdio for the set of its working directory
// (docs/design/agents.html#mcp).
//
// # Execution model
//
// A tool call runs the same core as the CLI: the server builds the argument
// vector of the repoctl command of the tool from validated input values and
// runs the command tree of repoctl in the server process, with --json, on
// a fresh cli.App whose streams are buffers (a Runner). The envelope that
// the command writes is the structured result of the call. The calls that
// run in the process are serialized: one command runs at a time, so the
// command tree and its handlers need no locking of their own. A call that
// waits (repoctl_run_wait) releases the server between its polls.
//
// repoctl_build is the exception of the design: it starts repoctl build
// --json --progress none as a detached child in its own session, with the
// flags --config, --jobs and --lock-wait 0 and without the REPOCTL_*
// variables of the environment rule, and returns the Run at plan.ready.
// The run continues when the MCP session ends.
//
// No tool publishes, signs or pushes. The server checks each command line
// before it runs it (CheckCommand): the command path must be one that a
// tool maps to, never one of notExposed, and never of the safety class
// shared state; some flags are never passed.
package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
)

// The classes of a tool (model.html#safety). Shared state has no tool.
const (
	ClassRead       = "read"
	ClassStaging    = "staging"
	ClassLocalWrite = "local-write"
)

// Catalog is mcp-tools.json: the tools, the tool modes, the resources and
// the commands that are not exposed.
type Catalog struct {
	Server            ServerInfo         `json:"server"`
	Tools             []Tool             `json:"tools"`
	Resources         []Resource         `json:"resources"`
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NotExposed        []string           `json:"notExposed"`

	raw []byte
}

// ServerInfo is server of mcp-tools.json.
type ServerInfo struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Transport string `json:"transport"`
	ToolModes struct {
		Read []string `json:"read"`
	} `json:"toolModes"`
}

// Tool is one tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// OutputKind is the kind of the envelope; empty for repoctl_schema,
	// whose result is a schema.
	OutputKind  string      `json:"outputKind,omitempty"`
	Annotations Annotations `json:"annotations"`
	Class       string      `json:"class"`

	raw json.RawMessage
}

// Annotations are the MCP tool annotations.
type Annotations struct {
	ReadOnlyHint    bool  `json:"readOnlyHint"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  bool  `json:"idempotentHint"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// Resource is a fixed resource of the server.
type Resource struct {
	URI       string `json:"uri"`
	Name      string `json:"name"`
	MIMEType  string `json:"mimeType"`
	Subscribe bool   `json:"subscribe,omitempty"`
}

// ResourceTemplate is a resource with parameters.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	MIMEType    string `json:"mimeType"`
	Subscribe   bool   `json:"subscribe,omitempty"`
}

// Load reads mcp-tools.json of the contract that the binary embeds.
func Load() (*Catalog, error) {
	return Parse(api.SchemaFile("mcp-tools.json"))
}

// MustLoad is Load for the embedded file, which a test keeps valid.
func MustLoad() *Catalog {
	c, err := Load()
	if err != nil {
		panic(err)
	}
	return c
}

// Parse reads a catalog and checks that the classes and the tool mode read
// agree.
func Parse(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("mcp-tools.json: %w", err)
	}
	var raw struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	read := map[string]bool{}
	for _, n := range c.Server.ToolModes.Read {
		read[n] = true
	}
	seen := map[string]bool{}
	for i := range c.Tools {
		t := &c.Tools[i]
		t.raw = raw.Tools[i]
		if seen[t.Name] {
			return nil, fmt.Errorf("mcp-tools.json: tool %s twice", t.Name)
		}
		seen[t.Name] = true
		switch t.Class {
		case ClassRead, ClassStaging, ClassLocalWrite:
		default:
			return nil, fmt.Errorf("mcp-tools.json: tool %s has class %q", t.Name, t.Class)
		}
		if (t.Class == ClassRead) != read[t.Name] {
			return nil, fmt.Errorf("mcp-tools.json: tool %s has class %s, but toolModes.read says otherwise", t.Name, t.Class)
		}
	}
	c.raw = b
	return &c, nil
}

// Tool gives the tool with this name.
func (c *Catalog) Tool(name string) (Tool, bool) {
	for _, t := range c.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Names gives the names of the tools in the order of the file.
func (c *Catalog) Names() []string {
	out := make([]string, len(c.Tools))
	for i, t := range c.Tools {
		out[i] = t.Name
	}
	return out
}

// Mode is the tool mode of a server: --tools read|all|NAME,...
type Mode struct {
	// Spec is the value of --tools.
	Spec  string
	names map[string]bool
}

// ParseMode reads the value of --tools. A name that is not in the catalog
// is an error, which the CLI turns into a usage error.
func ParseMode(spec string, c *Catalog) (Mode, error) {
	m := Mode{Spec: spec, names: map[string]bool{}}
	switch strings.TrimSpace(spec) {
	case "", "all":
		m.Spec = "all"
		for _, t := range c.Tools {
			m.names[t.Name] = true
		}
		return m, nil
	case "read":
		for _, t := range c.Tools {
			if t.Class == ClassRead {
				m.names[t.Name] = true
			}
		}
		return m, nil
	}
	var unknown []string
	for _, n := range strings.Split(spec, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := c.Tool(n); !ok {
			unknown = append(unknown, n)
			continue
		}
		m.names[n] = true
	}
	if len(unknown) > 0 {
		return m, fmt.Errorf("unknown tool %s in --tools (the tools: read, all, or names of repoctl mcp tools)", strings.Join(unknown, ", "))
	}
	if len(m.names) == 0 {
		return m, fmt.Errorf("--tools names no tool")
	}
	return m, nil
}

// Allows reports whether the mode has the tool.
func (m Mode) Allows(name string) bool { return m.names[name] }

// Names gives the tools of the mode, sorted.
func (m Mode) Names() []string {
	out := make([]string, 0, len(m.names))
	for n := range m.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// JSON gives the tool definitions of the mode in the form of
// mcp-tools.json; in mode all, the file itself.
func (c *Catalog) JSON(m Mode) []byte {
	if m.Spec == "all" {
		return c.raw
	}
	var doc struct {
		Server            json.RawMessage   `json:"server"`
		Tools             []json.RawMessage `json:"tools"`
		Resources         json.RawMessage   `json:"resources"`
		ResourceTemplates json.RawMessage   `json:"resourceTemplates"`
		NotExposed        json.RawMessage   `json:"notExposed"`
	}
	if err := json.Unmarshal(c.raw, &doc); err != nil {
		panic(err)
	}
	doc.Tools = doc.Tools[:0]
	for _, t := range c.Tools {
		if m.Allows(t.Name) {
			doc.Tools = append(doc.Tools, t.raw)
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}
