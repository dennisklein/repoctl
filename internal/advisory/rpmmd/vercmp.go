// Package rpmmd reads what the advisory checks need from rpm-md
// repositories: the packages of primary.xml with their provides, requires
// and files, and the RPM version order (rpmvercmp) to compare versions and
// capabilities.
package rpmmd

import (
	"strconv"
	"strings"
)

// Vercmp compares two version or release strings in the RPM version order
// (rpmvercmp of rpm 4.18): -1 when a is older, 0 when equal, 1 when newer.
// A tilde sorts before everything, a caret after the end of a version.
func Vercmp(a, b string) int {
	if a == b {
		return 0
	}
	one, two := a, b
	for len(one) > 0 || len(two) > 0 {
		for len(one) > 0 && !isAlnum(one[0]) && one[0] != '~' && one[0] != '^' {
			one = one[1:]
		}
		for len(two) > 0 && !isAlnum(two[0]) && two[0] != '~' && two[0] != '^' {
			two = two[1:]
		}
		// A tilde sorts before everything, also before the end.
		if strings.HasPrefix(one, "~") || strings.HasPrefix(two, "~") {
			if !strings.HasPrefix(one, "~") {
				return 1
			}
			if !strings.HasPrefix(two, "~") {
				return -1
			}
			one, two = one[1:], two[1:]
			continue
		}
		// A caret sorts after the end, but before everything else.
		if strings.HasPrefix(one, "^") || strings.HasPrefix(two, "^") {
			if one == "" {
				return -1
			}
			if two == "" {
				return 1
			}
			if !strings.HasPrefix(one, "^") {
				return 1
			}
			if !strings.HasPrefix(two, "^") {
				return -1
			}
			one, two = one[1:], two[1:]
			continue
		}
		if one == "" || two == "" {
			break
		}
		num := isDigit(one[0])
		seg1, rest1 := segment(one, num)
		seg2, rest2 := segment(two, num)
		if seg2 == "" {
			// Segments of different types: a number is newer.
			if num {
				return 1
			}
			return -1
		}
		if num {
			seg1 = strings.TrimLeft(seg1, "0")
			seg2 = strings.TrimLeft(seg2, "0")
			if len(seg1) != len(seg2) {
				if len(seg1) > len(seg2) {
					return 1
				}
				return -1
			}
		}
		if c := strings.Compare(seg1, seg2); c != 0 {
			return c
		}
		one, two = rest1, rest2
	}
	switch {
	case one == "" && two == "":
		return 0
	case one == "":
		return -1
	}
	return 1
}

func segment(s string, num bool) (string, string) {
	i := 0
	for i < len(s) && ((num && isDigit(s[i])) || (!num && isAlpha(s[i]))) {
		i++
	}
	return s[:i], s[i:]
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

// EVR is an epoch, a version and a release.
type EVR struct {
	Epoch   string
	Version string
	Release string
}

// ParseEVR reads [EPOCH:]VERSION[-RELEASE].
func ParseEVR(s string) EVR {
	var e EVR
	if i := strings.IndexByte(s, ':'); i >= 0 {
		e.Epoch, s = s[:i], s[i+1:]
	}
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		e.Version, e.Release = s[:i], s[i+1:]
	} else {
		e.Version = s
	}
	return e
}

// String gives [EPOCH:]VERSION[-RELEASE] without an epoch of 0.
func (e EVR) String() string {
	s := e.Version
	if e.Release != "" {
		s += "-" + e.Release
	}
	if e.Epoch != "" && e.Epoch != "0" {
		s = e.Epoch + ":" + s
	}
	return s
}

func epochOf(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// CompareEVR compares two EVRs: the epoch, the version, and the release
// when both have one.
func CompareEVR(a, b EVR) int {
	if ea, eb := epochOf(a.Epoch), epochOf(b.Epoch); ea != eb {
		if ea > eb {
			return 1
		}
		return -1
	}
	if c := Vercmp(a.Version, b.Version); c != 0 {
		return c
	}
	if a.Release == "" || b.Release == "" {
		return 0
	}
	return Vercmp(a.Release, b.Release)
}
