package api

import (
	"fmt"
	"strings"
	"time"
)

// APIVersion is the one version string of the --json output, the MCP
// structured content and the stored set files.
const APIVersion = "repoctl/v1alpha1"

// ReadAPIVersions lists the apiVersions that this binary reads.
var ReadAPIVersions = []string{APIVersion}

// JournalVersion is the version v of each journal line.
const JournalVersion = 1

// FormatTime gives t in the form of the contract: RFC 3339 in UTC with
// milliseconds and a Z suffix. The zero time gives "".
func FormatTime(t time.Time) Time {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ParseTime reads a Time of the contract.
func ParseTime(s Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// MustTime is ParseTime for values that this binary wrote; it gives the zero
// time on an error.
func MustTime(s Time) time.Time {
	t, _ := ParseTime(s)
	return t
}

// Node gives the id PKG@TARGET of a node.
func Node(pkg PackageId, target TargetId) NodeId { return pkg + "@" + target }

// SplitNode gives the package and the target of a node id.
func SplitNode(n NodeId) (PackageId, TargetId) {
	pkg, target, _ := strings.Cut(n, "@")
	return pkg, target
}

// ShortCommit gives the 7 hex digits that the terminal and the UI show.
func ShortCommit(c Commit) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// ShortDigest gives the form sha256:9d07… of a digest.
func ShortDigest(d Digest) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) > 4 {
		return "sha256:" + h[:4] + "…"
	}
	return d
}

// ShortFingerprint gives the last 8 digits of a fingerprint in two groups,
// for example "…41D8 0B6E".
func ShortFingerprint(f string) string {
	if len(f) < 8 {
		return f
	}
	t := f[len(f)-8:]
	return "…" + t[:4] + " " + t[4:]
}

// FormatDuration gives the terminal form of a duration: 4.2s, 26s, 6m53s,
// 41m07s, 1h12m.
func FormatDuration(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		if m < 10 {
			return fmt.Sprintf("%dm%02ds", m, s)
		}
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}

// NewEnvelope gives an envelope of the kind with the metadata.
func NewEnvelope(kind Kind, md Metadata) Envelope {
	return Envelope{APIVersion: APIVersion, Kind: kind, Metadata: md, Items: []any{}, Warnings: []Problem{}, Errors: []Problem{}}
}
