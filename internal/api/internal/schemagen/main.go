// Command schemagen writes the Go types of internal/api from the JSON
// Schemas of the machine contract in docs/design/contract/schema.
//
// The schema files are the source of the contract. schemagen copies them
// into internal/api/schema, so that the binary can embed them, and writes
// zz_output.go and zz_events.go with one Go type per $defs entry. A test
// runs schemagen again and fails when the result differs.
//
// Usage (from internal/api): go run ./internal/schemagen
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// node is one JSON value with the key order of the file.
type node struct {
	kind  byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	keys  []string
	obj   map[string]*node
	arr   []*node
	str   string
	num   json.Number
	boolV bool
}

func parse(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: 'o', obj: map[string]*node{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				v, err := parse(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.obj[k] = v
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &node{kind: 'a'}
			for dec.More() {
				v, err := parse(dec)
				if err != nil {
					return nil, err
				}
				n.arr = append(n.arr, v)
			}
			_, err := dec.Token()
			return n, err
		}
	case string:
		return &node{kind: 's', str: t}, nil
	case json.Number:
		return &node{kind: 'n', num: t}, nil
	case bool:
		return &node{kind: 'b', boolV: t}, nil
	case nil:
		return &node{kind: 'z'}, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

func (n *node) get(k string) *node {
	if n == nil || n.kind != 'o' {
		return nil
	}
	return n.obj[k]
}

func (n *node) strs() []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, v := range n.arr {
		if v.kind == 's' {
			out = append(out, v.str)
		}
	}
	return out
}

// initialisms of the Go names.
var initialisms = map[string]string{
	"id": "ID", "url": "URL", "api": "API", "uid": "UID", "pid": "PID",
	"nvr": "NVR", "nvra": "NVRA", "nvrs": "NVRs", "evr": "EVR", "rpm": "RPM",
	"rpms": "RPMs", "srpm": "SRPM", "sbom": "SBOM", "json": "JSON", "mcp": "MCP",
	"ui": "UI", "uri": "URI", "abi": "ABI", "oci": "OCI", "os": "OS", "gc": "GC",
	"el": "EL", "ms": "Ms", "purl": "PURL", "sha256": "Sha256",
}

func goName(s string) string {
	// Split on non-alphanumerics and on lower-to-upper case changes.
	var parts []string
	cur := []rune{}
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, string(cur))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) && unicode.IsLower(rs[i-1]) {
			flush()
		}
		cur = append(cur, r)
	}
	flush()
	var b strings.Builder
	for _, p := range parts {
		lp := strings.ToLower(p)
		if v, ok := initialisms[lp]; ok {
			b.WriteString(v)
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	out := b.String()
	if out == "" {
		out = "X"
	}
	if unicode.IsDigit([]rune(out)[0]) {
		out = "N" + out
	}
	return out
}

func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return s[:len(s)-1]
	}
	return s + "Item"
}

type gen struct {
	defs     *node // $defs of this file
	extDefs  *node // $defs of output.schema.json, for events
	out      bytes.Buffer
	pending  []pendingType
	declared map[string]bool
	scalar   map[string]string // $defs name -> Go type of scalar defs
	// header names the properties of the event header, which the event
	// structs embed.
	header map[string]bool
	events bool
	// eventTypes names the $defs of the event types (the then of allOf).
	// Only these embed the header; a nested object of an event does not.
	eventTypes map[string]bool
	// used holds the names of $defs and of the inline types so far.
	used map[string]bool
}

// inlineName gives a free type name for an inline object.
func (g *gen) inlineName(name string) string {
	for g.used[name] {
		name += "Item"
	}
	g.used[name] = true
	return name
}

type pendingType struct {
	name   string
	schema *node
	doc    string
}

func (g *gen) printf(format string, a ...any) { fmt.Fprintf(&g.out, format, a...) }

func comment(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(wrap(text, 76), "\n") {
		b.WriteString("// " + line + "\n")
	}
	return b.String()
}

func wrap(s string, n int) string {
	var lines []string
	var cur string
	for _, w := range strings.Fields(s) {
		if cur != "" && len(cur)+1+len(w) > n {
			lines = append(lines, cur)
			cur = w
			continue
		}
		if cur == "" {
			cur = w
		} else {
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n")
}

func refName(ref string) (string, bool) {
	i := strings.LastIndex(ref, "/")
	return ref[i+1:], strings.HasPrefix(ref, "output.json#")
}

// isNullable reports a oneOf of null and one other schema, and returns it.
func nullable(s *node) (*node, bool) {
	if one := s.get("oneOf"); one != nil && len(one.arr) == 2 {
		a, b := one.arr[0], one.arr[1]
		if t := a.get("type"); t != nil && t.kind == 's' && t.str == "null" {
			return b, true
		}
		if t := b.get("type"); t != nil && t.kind == 's' && t.str == "null" {
			return a, true
		}
	}
	if t := s.get("type"); t != nil && t.kind == 'a' {
		ts := t.strs()
		if len(ts) == 2 && slices.Contains(ts, "null") {
			other := ts[0]
			if other == "null" {
				other = ts[1]
			}
			return &node{kind: 'o', keys: []string{"type"}, obj: map[string]*node{"type": {kind: 's', str: other}}}, true
		}
	}
	return s, false
}

// goType returns the Go type of schema s for a field; ptrOK says whether a
// struct type takes a pointer (optional field).
func (g *gen) goType(s *node, name string, optional bool) (typ string, isStructOrPtr bool) {
	inner, isNull := nullable(s)
	if isNull {
		t, _ := g.goType(inner, name, false)
		if strings.HasPrefix(t, "[]") || strings.HasPrefix(t, "map[") || t == "any" || strings.HasPrefix(t, "*") {
			return t, true
		}
		return "*" + t, true
	}
	if ref := s.get("$ref"); ref != nil {
		rn, ext := refName(ref.str)
		if st, ok := g.scalar[rn]; ok {
			return st, false
		}
		isStruct := g.isStructDef(rn, ext)
		if ext && g.events {
			rn = "api." + rn
		}
		if isStruct && optional {
			return "*" + rn, true
		}
		return rn, isStruct
	}
	if s.get("enum") != nil || s.get("const") != nil {
		if c := s.get("const"); c != nil && c.kind == 'n' {
			return "int", false
		}
		return "string", false
	}
	t := s.get("type")
	ts := ""
	if t != nil && t.kind == 's' {
		ts = t.str
	}
	switch ts {
	case "string":
		return "string", false
	case "integer":
		return "int", false
	case "number":
		return "float64", false
	case "boolean":
		return "bool", false
	case "array":
		it := s.get("items")
		if it == nil {
			return "[]any", false
		}
		et, _ := g.goType(it, singular(name), false)
		return "[]" + et, false
	case "object":
		if ap := s.get("additionalProperties"); ap != nil && ap.kind == 'o' {
			vt, _ := g.goType(ap, name+"Value", false)
			return "map[string]" + vt, false
		}
		if s.get("properties") == nil {
			return "map[string]any", false
		}
		name = g.inlineName(name)
		g.pending = append(g.pending, pendingType{name: name, schema: s})
		if optional {
			return "*" + name, true
		}
		return name, true
	}
	if s.get("properties") != nil {
		name = g.inlineName(name)
		g.pending = append(g.pending, pendingType{name: name, schema: s})
		if optional {
			return "*" + name, true
		}
		return name, true
	}
	return "any", false
}

func (g *gen) isStructDef(name string, ext bool) bool {
	d := g.defs.get(name)
	if ext && g.extDefs != nil {
		d = g.extDefs.get(name)
	}
	if d == nil {
		return false
	}
	if d.get("properties") != nil {
		return true
	}
	if t := d.get("type"); t != nil && t.kind == 's' && t.str == "object" {
		return d.get("additionalProperties") == nil || d.get("additionalProperties").kind != 'o'
	}
	return false
}

// collectProps merges the properties of s and of its allOf entries that are
// plain (not if/then).
func collectProps(s *node) (keys []string, props map[string]*node, required []string) {
	props = map[string]*node{}
	add := func(x *node) {
		if p := x.get("properties"); p != nil {
			for _, k := range p.keys {
				if _, ok := props[k]; !ok {
					keys = append(keys, k)
				}
				props[k] = p.obj[k]
			}
		}
		required = append(required, x.get("required").strs()...)
	}
	add(s)
	if all := s.get("allOf"); all != nil {
		for _, x := range all.arr {
			if x.get("if") == nil {
				add(x)
			}
		}
	}
	return keys, props, required
}

func (g *gen) writeStruct(name string, s *node, doc string) {
	if g.declared[name] {
		return
	}
	g.declared[name] = true
	if doc == "" {
		if d := s.get("description"); d != nil {
			doc = d.str
		}
	}
	if doc != "" {
		g.printf("%s", comment(name+": "+doc))
	}
	keys, props, required := collectProps(s)
	g.printf("type %s struct {\n", name)
	embeds := g.events && g.eventTypes[name]
	if embeds {
		g.printf("\tEventHeader\n")
	}
	var nilSlices []string
	for _, k := range keys {
		if embeds && g.header[k] {
			continue
		}
		p := props[k]
		req := slices.Contains(required, k)
		fieldName := goName(k)
		typ, _ := g.goType(p, name+fieldName, !req)
		tag := k
		_, isNull := nullable(p)
		omit := !req
		if g.events && k == "exitCode" {
			omit = false
		}
		if omit {
			tag += ",omitempty"
		}
		if req && !isNull && (strings.HasPrefix(typ, "[]") || strings.HasPrefix(typ, "map[")) {
			nilSlices = append(nilSlices, fieldName+"\x00"+typ)
		}
		if d := p.get("description"); d != nil {
			g.printf("%s", indent(comment(d.str)))
		} else if e := p.get("enum"); e != nil {
			g.printf("\t// One of: %s.\n", strings.Join(e.strs(), ", "))
		}
		g.printf("\t%s %s `json:\"%s\"`\n", fieldName, typ, tag)
	}
	g.printf("}\n\n")
	if len(nilSlices) > 0 {
		g.printf("// MarshalJSON writes an empty list or object, never null, for each\n// required list of %s.\n", name)
		g.printf("func (v %s) MarshalJSON() ([]byte, error) {\n\ttype plain %s\n\tp := plain(v)\n", name, name)
		for _, ns := range nilSlices {
			f, t, _ := strings.Cut(ns, "\x00")
			g.printf("\tif p.%s == nil {\n\t\tp.%s = %s{}\n\t}\n", f, f, t)
		}
		g.printf("\treturn json.Marshal(p)\n}\n\n")
	}
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		if l == "" {
			continue
		}
		b.WriteString("\t" + l)
	}
	return b.String()
}

func (g *gen) writeEnum(name string, s *node) {
	g.declared[name] = true
	if d := s.get("description"); d != nil {
		g.printf("%s", comment(name+": "+d.str))
	} else {
		g.printf("// %s is an enum of the contract.\n", name)
	}
	g.printf("type %s string\n\n", name)
	vals := s.get("enum").strs()
	g.printf("// The values of %s.\nconst (\n", name)
	for _, v := range vals {
		g.printf("\t%s%s %s = %q\n", name, goName(v), name, v)
	}
	g.printf(")\n\n")
	g.printf("// %sValues lists the values of %s in schema order.\nvar %sValues = []%s{", name, name, name, name)
	for i, v := range vals {
		if i > 0 {
			g.printf(", ")
		}
		g.printf("%s%s", name, goName(v))
	}
	g.printf("}\n\n")
}

func (g *gen) run(names []string) {
	for _, name := range names {
		s := g.defs.get(name)
		switch {
		case s.get("enum") != nil:
			g.writeEnum(name, s)
		case g.scalar[name] != "":
			// scalar alias, written by writeScalars
		default:
			g.writeStruct(name, s, "")
		}
		for len(g.pending) > 0 {
			p := g.pending[0]
			g.pending = g.pending[1:]
			g.writeStruct(p.name, p.schema, p.doc)
		}
	}
}

func scalarOf(s *node) string {
	if s.get("enum") != nil || s.get("properties") != nil || s.get("oneOf") != nil || s.get("allOf") != nil {
		return ""
	}
	t := s.get("type")
	if t == nil || t.kind != 's' {
		return ""
	}
	switch t.str {
	case "string":
		return "string"
	case "integer":
		return "int"
	}
	return ""
}

func main() {
	src := flag.String("src", "../../docs/design/contract/schema", "directory of the schema files")
	dst := flag.String("dst", ".", "directory of package api")
	flag.Parse()

	files := []string{"output.schema.json", "events.schema.json", "mcp-tools.json"}
	raw := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(*src, f))
		must(err)
		raw[f] = b
		must(os.MkdirAll(filepath.Join(*dst, "schema"), 0o755))
		must(os.WriteFile(filepath.Join(*dst, "schema", f), b, 0o644))
	}

	out := load(raw["output.schema.json"])
	ev := load(raw["events.schema.json"])

	// output.schema.json
	g := &gen{defs: out.get("$defs"), declared: map[string]bool{}, scalar: map[string]string{}, used: map[string]bool{}}
	for _, k := range g.defs.keys {
		g.used[k] = true
	}
	for _, k := range g.defs.keys {
		if st := scalarOf(g.defs.obj[k]); st != "" {
			g.scalar[k] = k
		}
	}
	header(&g.out, "api", "output.schema.json", []string{"encoding/json"})
	writeScalars(g)
	g.run(g.defs.keys)
	write(filepath.Join(*dst, "zz_output.go"), g.out.Bytes())

	// events.schema.json: one struct per event type, each embeds the header.
	ge := &gen{defs: ev.get("$defs"), extDefs: g.defs, declared: map[string]bool{}, scalar: map[string]string{}, events: true, header: map[string]bool{}, used: map[string]bool{}, eventTypes: map[string]bool{}}
	for _, x := range ev.get("allOf").arr {
		rn, _ := refName(x.get("then").get("$ref").str)
		ge.eventTypes[rn] = true
	}
	for k := range g.used {
		ge.used[k] = true
	}
	for _, k := range ge.defs.keys {
		ge.used[k] = true
	}
	for k := range g.scalar {
		ge.scalar[k] = "api." + k
	}
	for _, k := range ev.get("properties").keys {
		ge.header[k] = true
	}
	var eb bytes.Buffer
	header(&eb, "api", "events.schema.json", []string{"encoding/json"})
	ge.out = eb
	// The header struct.
	hdr := &node{kind: 'o', keys: []string{"properties", "required"}, obj: map[string]*node{"properties": ev.get("properties"), "required": ev.get("required")}}
	ge.writeStruct("EventHeader", hdr, "the keys of every journal line. A reader skips a line whose type it does not know.")
	ge.run(ge.defs.keys)
	// Map of event type to struct.
	ge.printf("// EventStructs maps each event type to a new value of its struct.\nvar EventStructs = map[EventType]func() any{\n")
	for _, x := range ev.get("allOf").arr {
		t := x.get("if").get("properties").get("type").get("const").str
		rn, _ := refName(x.get("then").get("$ref").str)
		ge.printf("\tEventType%s: func() any { return new(%s) },\n", goName(t), rn)
	}
	ge.printf("}\n")
	src2 := strings.ReplaceAll(ge.out.String(), "api.", "")
	if !strings.Contains(strings.SplitN(src2, ")\n", 2)[1], "json.") {
		src2 = strings.Replace(src2, "import (\n\t\"encoding/json\"\n)\n", "", 1)
	}
	write(filepath.Join(*dst, "zz_events.go"), []byte(src2))
}

func writeScalars(g *gen) {
	var names []string
	for k := range g.scalar {
		names = append(names, k)
	}
	sort.Strings(names)
	g.printf("// Scalar types of the contract. They are aliases, so a plain string\n// or int converts without a cast.\ntype (\n")
	for _, k := range names {
		s := g.defs.get(k)
		if d := s.get("description"); d != nil {
			g.printf("%s", indent(comment(k+": "+d.str)))
		}
		g.printf("\t%s = %s\n", k, scalarOf(s))
	}
	g.printf(")\n\n")
}

func load(b []byte) *node {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, err := parse(dec)
	must(err)
	return n
}

func header(w io.Writer, pkg, file string, imports []string) {
	fmt.Fprintf(w, "// Code generated by schemagen from %s. DO NOT EDIT.\n\npackage %s\n\n", file, pkg)
	if len(imports) > 0 {
		fmt.Fprintf(w, "import (\n")
		for _, i := range imports {
			fmt.Fprintf(w, "\t%q\n", i)
		}
		fmt.Fprintf(w, ")\n\n")
	}
}

func write(path string, src []byte) {
	formatted, err := format.Source(src)
	if err != nil {
		_ = os.WriteFile(path+".broken", src, 0o644)
		fmt.Fprintln(os.Stderr, "format:", err)
		os.Exit(1)
	}
	must(os.WriteFile(path, formatted, 0o644))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}
