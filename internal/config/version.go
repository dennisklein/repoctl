package config

import (
	"strconv"
	"strings"
)

// Version and Commit of this binary. The build sets them with -ldflags
// "-X github.com/dennisklein/repoctl/internal/config.Version=…"; the CLI
// falls back to the build info of the Go toolchain.
var (
	Version = "0.1.0-dev"
	Commit  = ""
)

// VersionLess reports a < b for versions of the form [v]MAJOR.MINOR.PATCH[-PRE].
// A pre-release sorts before its release.
func VersionLess(a, b string) bool {
	pa, prea := splitVersion(a)
	pb, preb := splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	switch {
	case prea == preb:
		return false
	case prea == "":
		return false
	case preb == "":
		return true
	}
	return prea < preb
}

func splitVersion(v string) ([3]int, string) {
	v = strings.TrimPrefix(v, "v")
	v, pre, _ := strings.Cut(v, "-")
	v, _, _ = strings.Cut(v, "+")
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out, pre
}
