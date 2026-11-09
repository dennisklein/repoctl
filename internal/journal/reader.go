package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/dennisklein/repoctl/internal/api"
)

// Line is one decoded line of a journal.
type Line struct {
	api.EventHeader
	// Event is a pointer to the struct of the type, nil for a type that
	// this binary does not know: a reader skips such a line.
	Event api.Event
}

// Decode reads one line of a journal.
func Decode(b []byte) (Line, error) {
	var l Line
	if err := json.Unmarshal(b, &l.EventHeader); err != nil {
		return l, fmt.Errorf("journal line: %w", err)
	}
	if l.V != api.JournalVersion {
		return l, fmt.Errorf("journal line: version %d", l.V)
	}
	mk, ok := api.EventStructs[l.Type]
	if !ok {
		return l, nil
	}
	ev := mk().(api.Event)
	if err := json.Unmarshal(b, ev); err != nil {
		return l, fmt.Errorf("journal line %d (%s): %w", l.Seq, l.Type, err)
	}
	l.Event = ev
	return l, nil
}

// ReadAll reads the lines of a journal. A line that cannot be decoded, or
// that has an unknown type, is skipped, as is a last line without its
// newline (it is still being written).
func ReadAll(r io.Reader) ([]Line, error) {
	var out []Line
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		b, err := br.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
		b = bytes.TrimSpace(b)
		if len(b) == 0 {
			continue
		}
		l, derr := Decode(b)
		if derr != nil || l.Event == nil {
			continue
		}
		out = append(out, l)
	}
}

// Read reads the journal at path.
func Read(path string) ([]Line, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadAll(f)
}
