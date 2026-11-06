package manifest

import (
	"strings"
	"unicode"
)

// Vercmp compares two version or release strings with the rules of
// rpmvercmp: it gives -1, 0 or 1.
func Vercmp(a, b string) int {
	if a == b {
		return 0
	}
	isAlnum := func(r byte) bool { return r < 0x80 && (unicode.IsLetter(rune(r)) || unicode.IsDigit(rune(r))) }
	isDigit := func(r byte) bool { return r >= '0' && r <= '9' }
	for len(a) > 0 || len(b) > 0 {
		for len(a) > 0 && !isAlnum(a[0]) && a[0] != '~' && a[0] != '^' {
			a = a[1:]
		}
		for len(b) > 0 && !isAlnum(b[0]) && b[0] != '~' && b[0] != '^' {
			b = b[1:]
		}
		// The tilde sorts before everything, also the end of the string.
		if strings.HasPrefix(a, "~") || strings.HasPrefix(b, "~") {
			if !strings.HasPrefix(a, "~") {
				return 1
			}
			if !strings.HasPrefix(b, "~") {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		// The caret sorts after the end of the string, before anything else.
		if strings.HasPrefix(a, "^") || strings.HasPrefix(b, "^") {
			if len(a) == 0 {
				return -1
			}
			if len(b) == 0 {
				return 1
			}
			if !strings.HasPrefix(a, "^") {
				return 1
			}
			if !strings.HasPrefix(b, "^") {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if len(a) == 0 || len(b) == 0 {
			break
		}
		var sa, sb string
		numeric := isDigit(a[0])
		take := func(s string, f func(byte) bool) (string, string) {
			i := 0
			for i < len(s) && f(s[i]) {
				i++
			}
			return s[:i], s[i:]
		}
		isAlpha := func(r byte) bool { return isAlnum(r) && !isDigit(r) }
		if numeric {
			sa, a = take(a, isDigit)
			sb, b = take(b, isDigit)
		} else {
			sa, a = take(a, isAlpha)
			sb, b = take(b, isAlpha)
		}
		if sb == "" {
			// Segments of different types: the numeric one is newer.
			if numeric {
				return 1
			}
			return -1
		}
		if numeric {
			sa = strings.TrimLeft(sa, "0")
			sb = strings.TrimLeft(sb, "0")
			if len(sa) != len(sb) {
				if len(sa) > len(sb) {
					return 1
				}
				return -1
			}
		}
		if c := strings.Compare(sa, sb); c != 0 {
			return c
		}
	}
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return -1
	}
	return 1
}

// CompareEVR compares epoch, version and release with the rpm version
// compare: it gives -1, 0 or 1.
func CompareEVR(e1 int, v1, r1 string, e2 int, v2, r2 string) int {
	switch {
	case e1 < e2:
		return -1
	case e1 > e2:
		return 1
	}
	if c := Vercmp(v1, v2); c != 0 {
		return c
	}
	return Vercmp(r1, r2)
}

// CompareNVR compares two N-V-R strings of the same name by version and
// release, with epoch 0.
func CompareNVR(a, b string) int {
	_, va, ra := SplitNVR(a)
	_, vb, rb := SplitNVR(b)
	return CompareEVR(0, va, ra, 0, vb, rb)
}
