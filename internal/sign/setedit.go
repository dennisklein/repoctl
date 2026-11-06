package sign

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// The edits of repoctl.yaml keep each comment, each blank line and the
// layout of the file: yaml.v3 finds the nodes and their lines, and the edit
// changes or inserts only the lines of sign.keys. The result must parse as a
// set config.

// setOrder is the order of the top-level keys of repoctl.yaml after sign.
var setOrder = []string{"retention", "targets", "defaults", "packages", "skills"}

// AddKeyEntry adds an entry to sign.keys of repoctl.yaml. When the file has
// no sign.release.keyfile, it also adds keyfile (when keyfile is not "").
func AddKeyEntry(data []byte, k config.Key, keyfile string) ([]byte, error) {
	root, err := parseRoot(data)
	if err != nil {
		return nil, err
	}
	lines := splitLines(data)
	item := func(indent int) []string {
		pad := strings.Repeat(" ", indent)
		return []string{
			pad + "- id: " + k.ID,
			pad + "  file: " + k.File,
			pad + "  fingerprint: " + k.Fingerprint,
			pad + "  state: " + k.State,
		}
	}
	signKey, sign := mapGet(root, "sign")
	switch {
	case sign == nil:
		block := append([]string{"sign:", "  keys:"}, item(4)...)
		if keyfile != "" {
			block = append(block, "  release:", "    keyfile: "+keyfile)
		}
		at := len(lines)
		for _, name := range setOrder {
			if kn, _ := mapGet(root, name); kn != nil {
				at = startWithComments(lines, kn.Line-1)
				break
			}
		}
		if at == len(lines) {
			if at > 0 && strings.TrimSpace(lines[at-1]) != "" {
				block = append([]string{""}, block...)
			}
		} else {
			block = append(block, "")
			if at > 0 && strings.TrimSpace(lines[at-1]) != "" {
				block = append([]string{""}, block...)
			}
		}
		lines = insert(lines, at, block...)
	case sign.Kind == yaml.ScalarNode && sign.Tag == "!!null":
		// An empty "sign:" line.
		ind := signKey.Column - 1 + 2
		block := append([]string{strings.Repeat(" ", ind) + "keys:"}, item(ind+2)...)
		if keyfile != "" {
			block = append(block, strings.Repeat(" ", ind)+"release:", strings.Repeat(" ", ind+2)+"keyfile: "+keyfile)
		}
		lines = insert(lines, signKey.Line, block...)
	case sign.Kind != yaml.MappingNode || sign.Style&yaml.FlowStyle != 0:
		return nil, editError("sign is not a block mapping")
	default:
		childIndent := signKey.Column - 1 + 2
		if len(sign.Content) > 0 {
			childIndent = sign.Content[0].Column - 1
		}
		_, keys := mapGet(sign, "keys")
		var at int
		var block []string
		switch {
		case keys == nil:
			at = nodeEnd(sign)
			block = append([]string{strings.Repeat(" ", childIndent) + "keys:"}, item(childIndent+2)...)
			if len(sign.Content) > 0 {
				// Put keys first, under the line of sign:.
				at = signKey.Line
			}
		case keys.Kind != yaml.SequenceNode:
			return nil, editError("sign.keys is not a list")
		case keys.Style&yaml.FlowStyle != 0:
			if len(keys.Content) > 0 {
				return nil, editError("sign.keys is a flow list; write it as a block list")
			}
			// keys: [] becomes a block list.
			l := keys.Line - 1
			lines[l] = strings.Replace(lines[l], "[]", "", 1)
			lines[l] = strings.TrimRight(lines[l], " ")
			at = keys.Line
			kk, _ := mapGet(sign, "keys")
			block = item(kk.Column - 1 + 2)
		default:
			indent := keys.Content[0].Column - 1 - 2
			at = nodeEnd(keys)
			block = item(indent)
		}
		lines = insert(lines, at, block...)
		if keyfile != "" {
			out := joinLines(lines)
			root, err = parseRoot(out)
			if err != nil {
				return nil, err
			}
			_, sign = mapGet(root, "sign")
			_, rel := mapGet(sign, "release")
			if rel == nil {
				lines = insert(lines, nodeEnd(sign), strings.Repeat(" ", childIndent)+"release:", strings.Repeat(" ", childIndent+2)+"keyfile: "+keyfile)
			} else if _, kf := mapGet(rel, "keyfile"); kf == nil {
				if rel.Kind != yaml.MappingNode || rel.Style&yaml.FlowStyle != 0 {
					return nil, editError("sign.release is not a block mapping")
				}
				ind := childIndent + 2
				if len(rel.Content) > 0 {
					ind = rel.Content[0].Column - 1
				}
				lines = insert(lines, nodeEnd(rel), strings.Repeat(" ", ind)+"keyfile: "+keyfile)
			}
		}
	}
	return checked(joinLines(lines))
}

// SetKeyState sets sign.keys[].state of the key id in repoctl.yaml.
func SetKeyState(data []byte, id, state string) ([]byte, error) {
	root, err := parseRoot(data)
	if err != nil {
		return nil, err
	}
	_, sign := mapGet(root, "sign")
	_, keys := mapGet(sign, "keys")
	if keys == nil || keys.Kind != yaml.SequenceNode {
		return nil, editError("sign.keys is missing")
	}
	lines := splitLines(data)
	for _, item := range keys.Content {
		_, idNode := mapGet(item, "id")
		if idNode == nil || idNode.Value != id {
			continue
		}
		if item.Style&yaml.FlowStyle != 0 {
			return nil, editError(fmt.Sprintf("the entry %s of sign.keys is a flow mapping; write it as a block mapping", id))
		}
		_, st := mapGet(item, "state")
		if st == nil {
			idKey, _ := mapGet(item, "id")
			lines = insert(lines, nodeEnd(item), strings.Repeat(" ", idKey.Column-1)+"state: "+state)
			return checked(joinLines(lines))
		}
		l := st.Line - 1
		line := lines[l]
		col := st.Column - 1
		rest := line[col:]
		next := line[:col] + state
		if i := strings.Index(rest, " #"); i >= 0 {
			// Keep the comment in its column when the value fits.
			hash := col + i + 1
			pad := max(1, hash-len(next))
			next += strings.Repeat(" ", pad) + rest[i+1:]
		}
		lines[l] = next
		return checked(joinLines(lines))
	}
	return nil, editError("sign.keys has no key " + id)
}

func editError(msg string) error {
	return api.Errorf(api.PConfigInvalid, "%s: %s; repoctl cannot edit it", config.SetFile, msg).WithPath(config.SetFile).
		WithHint("correct the file", "repoctl schema config")
}

func parseRoot(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, editError(err.Error())
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, editError("the file is not a mapping")
	}
	return doc.Content[0], nil
}

// mapGet gives the key node and the value node of a mapping.
func mapGet(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// nodeEnd gives the 1-based line of the last node below n: an insert at
// this index of the 0-based lines goes after n.
func nodeEnd(n *yaml.Node) int {
	l := n.Line
	for _, c := range n.Content {
		if x := nodeEnd(c); x > l {
			l = x
		}
	}
	return l
}

// startWithComments gives the 0-based line where the comment lines above
// line i start.
func startWithComments(lines []string, i int) int {
	for i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
		i--
	}
	return i
}

func splitLines(data []byte) []string {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

func insert(lines []string, at int, add ...string) []string {
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	return append(out, lines[at:]...)
}

// checked parses the result as a set config.
func checked(data []byte) ([]byte, error) {
	if _, err := config.ParseSet(data, config.SetFile); err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return data, nil
}
