package ui

import (
	"embed"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/GSI-HPC/go-clikit/localweb/htmx"
)

// files holds the static files (style sheets, scripts, fonts) and the
// templates of the pages.
//
//go:embed static templates
var files embed.FS

// staticFS gives the static files with the vendored htmx files beside them,
// under one root, so that one hash versions them all.
func staticFS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the directory is embedded
	}
	return unionFS{sub, htmx.Files()}
}

// templateFS gives the templates.
func templateFS() fs.FS {
	sub, err := fs.Sub(files, "templates")
	if err != nil {
		panic(err)
	}
	return sub
}

// unionFS reads a name from the first file system that has it; a
// directory lists the entries of all.
type unionFS []fs.FS

func (u unionFS) Open(name string) (fs.File, error) {
	var first error
	for _, f := range u {
		file, err := f.Open(name)
		if err == nil {
			return file, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}

func (u unionFS) ReadDir(name string) ([]fs.DirEntry, error) {
	var out []fs.DirEntry
	seen := map[string]bool{}
	found := false
	for _, f := range u {
		es, err := fs.ReadDir(f, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found = true
		for _, e := range es {
			if !seen[e.Name()] {
				seen[e.Name()] = true
				out = append(out, e)
			}
		}
	}
	if !found {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}
