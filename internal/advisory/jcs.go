package advisory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"unicode/utf16"

	"github.com/dennisklein/repoctl/internal/api"
)

// Canonical gives the JSON Canonicalization Scheme form (RFC 8785) of v.
// v is first encoded with encoding/json, so typed facts and maps give the
// same bytes.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := writeJCS(&b, tree); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// FactsDigest gives sha256: plus the hex SHA-256 of the JCS form of facts
// (advisories.html#facts). No other field goes into the digest.
func FactsDigest(facts any) (api.Digest, error) {
	c, err := Canonical(facts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func writeJCS(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return err
		}
		s, err := jcsNumber(f)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case string:
		writeJCSString(b, t)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJCS(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// RFC 8785 sorts the keys by their UTF-16 code units.
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJCSString(b, k)
			b.WriteByte(':')
			if err := writeJCS(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unexpected %T", v)
	}
	return nil
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// jcsNumber writes a number as ECMAScript Number.prototype.toString does.
func jcsNumber(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("jcs: %v is not a JSON number", f)
	}
	if f == 0 {
		return "0", nil
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	// Go writes 1e-07; ECMAScript writes 1e-7.
	mant, exp, _ := bytesCut(s, 'e')
	sign := exp[0]
	digits := exp[1:]
	for len(digits) > 1 && digits[0] == '0' {
		digits = digits[1:]
	}
	return mant + "e" + string(sign) + digits, nil
}

func bytesCut(s string, c byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func writeJCSString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
