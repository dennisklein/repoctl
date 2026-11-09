// Package api holds the Go types of the machine contract of repoctl: the
// output envelope, the kinds, the problem codes, the enums and the journal
// events (docs/design/contract.html).
//
// The JSON Schemas in docs/design/contract/schema are the source. schemagen
// writes zz_output.go and zz_events.go from them and copies them into
// schema/, where the binary embeds them. Run "go generate ./internal/api"
// after a change of a schema file; a test fails when the files differ.
package api

//go:generate go run ./internal/schemagen -src ../../docs/design/contract/schema -dst .
