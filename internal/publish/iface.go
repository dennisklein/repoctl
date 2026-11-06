// Package publish runs the publish runs of a set (publish.html#order):
// publish, restore, prune, resign and import. Each run has the same eight
// steps: check, sign, repodata, verify, verify-el, push, tag and cache.
// Steps 1 to 5 run for each target before the first push; steps 6 to 8
// run for one target after the other.
//
// The package reads what other parts of repoctl own through narrow
// interfaces: the staged nodes of the build runs (Stager), the signing key
// (Signer), gates 1 to 3 (Gates), the advisory gate (AdvisoryGate), the
// check in the verify root (ELVerifier) and the run journal (Journal). Each
// has a default implementation in this package; the CLI wires the real ones.
package publish

import (
	"context"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/records"
)

// Signer signs the RPMs and the repomd.xml files of a publish with the key
// of the human who publishes.
type Signer interface {
	// Info gives the signer: uid, full fingerprint (upper case hex) and
	// key id of sign.keys, if known.
	Info() api.Signer
	// Probe checks before the first signature that the key can sign now
	// (signing.html#probe): it signs a small test file.
	Probe(ctx context.Context) error
	// SignRPMs gives each RPM file a new signature, in place
	// (rpmsign --addsign or --resign).
	SignRPMs(ctx context.Context, files []string) error
	// SignDetached writes an armored detached signature of file to sig
	// (repomd.xml.asc).
	SignDetached(ctx context.Context, file, sig string) error
}

// StagedFile is one file of a staged node: the data of its artifact.added
// event and the path of the file in the staging of the run.
type StagedFile struct {
	// Kind is rpm, srpm, debuginfo or debugsource.
	Kind   string
	Repo   string
	NVRA   string
	File   string
	Path   string
	SHA256 string
	Size   int64
}

// StagedNode is a node with a build for the current lock in an
// unpublished run (build.html#staging).
type StagedNode struct {
	Node    api.NodeId
	Package string
	Target  string
	Run     string
	Commit  string
	Files   []StagedFile
	// Draft is the builds[] entry of the RepoManifest that the build step
	// drafted; publish sets publish, signer, records and the RPM names.
	Draft manifest.Build
	// Facts, SourceURIs and Buildroot feed the supply chain records.
	Facts      records.Facts
	SourceURIs map[string]string
	// Buildroot is the path of the buildroot file of the node, or "".
	Buildroot string
}

// NotReady is a node of a target that is neither current nor staged.
type NotReady struct {
	Node api.NodeId
	// State is the plan action or the node state: build, failed, blocked,
	// skip.
	State  string
	Reason string
}

// TargetNodes is the plan state of the nodes of one target.
type TargetNodes struct {
	Staged   []StagedNode
	Current  []api.NodeId
	NotReady []NotReady
}

// Stager gives the plan state of the nodes of a target for the current
// lock, against the RepoManifest of the base (nil for a first publish).
// run limits the staged nodes to one build run, if not "".
type Stager interface {
	Nodes(ctx context.Context, target string, base *manifest.Manifest, run string) (*TargetNodes, error)
}

// GateInput is what gates 1 to 3 read for one target of a publish run.
type GateInput struct {
	Set    *config.Set
	SetDir string
	Target string
	Kind   string
	Signer api.Signer
	// BaseDir is the local copy of the base, "" for a first publish.
	BaseDir string
	// BaseSigners counts the RPMs of the base per signing fingerprint.
	BaseSigners map[string]int
	// NewKeyfile is the key file of the release RPM with the highest NVR
	// after the publish, if the publish adds a release RPM, else "".
	NewKeyfile string
}

// GateResult is the result of gates 1 to 3 for one target.
type GateResult struct {
	Warnings []api.Problem
	// Trust is set when the key file changes: the trust block of the
	// summary (signing.html#trust-block). The human types each added key
	// id.
	Trust *manifest.Trust
	// Deployed is the line under the signer in the summary, for example
	// "deployed with slurm-release-26.05-2 (confirmed by d.klein 2026-09-10)".
	Deployed string
	// ClientKeys are the key files of the client trust set for verify-el.
	ClientKeys []string
	// TrustLines are the lines of the trust block, if any.
	TrustLines []string
}

// Gates runs gates 1 to 3, the check of the signers of the base, the key
// file and the .repo files (signing.html#gates). An error stops the
// publish in step 1.
type Gates interface {
	Check(ctx context.Context, in GateInput) (*GateResult, error)
}

// AdvisoryInput is what the advisory gate reads for one target.
type AdvisoryInput struct {
	Set    *config.Set
	Target string
	Kind   string
	// BaseDir is the local copy of the base, "" for a first publish.
	BaseDir string
	// Add are the RPM files that the publish adds, Remove the paths of the
	// base files that it removes.
	Add    []string
	Remove []string
	// Packages are the packages of the taken nodes; Owners maps the SRPM
	// file of each taken node to its package.
	Packages []string
	Owners   map[string]string
	// Accept are the ids of --accept-advisory, with Reason.
	Accept []string
	Reason string
	// User and Host record an acceptance.
	User, Host string
}

// AdvisoryResult is the result of the advisory gate for one target.
type AdvisoryResult struct {
	Gate     []api.RunPublishValueGateItem
	Accepted []manifest.Accepted
	// Line is the advisory line of the summary.
	Line     string
	Warnings []api.Problem
}

// AdvisoryGate runs the advisory gate (advisories.html#gate). It fails
// closed: an error stops the publish (advisory.open, advisory.not-blocking,
// prune.breaks-closure, advisory.gate-unavailable).
type AdvisoryGate interface {
	Gate(ctx context.Context, in AdvisoryInput) (*AdvisoryResult, error)
}

// ELVerifier runs step 5: the check of a target tree in the verify root of
// the target with the client trust set (signing.html#verify-el).
type ELVerifier interface {
	VerifyEL(ctx context.Context, target, treeDir string, keyFiles []string) error
}

// Journal appends typed events to the journal of the publish run. ev is
// one of the event structs of api (api.PublishStep, ...); the journal sets
// the header: v, run, seq, time and type.
type Journal interface {
	Append(typ api.EventType, ev any) error
	Close() error
}
