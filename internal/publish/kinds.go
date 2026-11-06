package publish

// The kinds of publish runs (publish.html#per-kind).
const (
	KindPublish = "publish"
	KindRestore = "restore"
	KindPrune   = "prune"
	KindResign  = "resign"
	KindImport  = "import"
	KindReseed  = "reseed"
)

// The values of Rules.LockUnpushed.
const (
	// LockAll checks the set commit and each pinned package commit.
	LockAll = "all"
	// LockSet checks the set commit only.
	LockSet = "set"
)

// The values of Rules.Freshness.
const (
	// FreshApplies runs the freshness check (state.*).
	FreshApplies = "applies"
	// FreshEmpty requires a registry without a snapshot of the target, else
	// import.not-empty.
	FreshEmpty = "empty"
)

// The values of Rules.Advisory.
const (
	AdvisoryBlocks  = "blocks"  // the gate blocks; --accept-advisory accepts
	AdvisoryClosure = "closure" // only a new broken requirement blocks (prune.breaks-closure)
	AdvisorySkip    = "skip"    // the NVRs and the payloads do not change
	AdvisoryReport  = "report"  // a report that does not block
)

// The values of Rules.BaseSigners.
const (
	SignersRefuse       = "refuse"         // removed, revoked or unknown signers refuse
	SignersResignRemove = "resign-removed" // RPMs of removed keys get a new signature
	SignersKept         = "kept"           // the rule applies to the kept RPMs
	SignersResignID     = "resign-id"      // RPMs of the key ID get a new signature
	SignersVerify       = "verify"         // each RPM verifies with sign.keys
)

// The values of Rules.Trust.
const (
	TrustNewKeyfile = "new-keyfile"    // when the release package has a new key file
	TrustNewest     = "newest-release" // when the newest release NVR has another key file than the base
	TrustNever      = "never"
)

// Rules are the checks and the actions of one publish kind: the table
// "Checks and steps per publish kind" of publish.html#per-kind. Each kind
// runs lock.dirty, gates 1 to 3, the signer probe, verify and verify-el.
type Rules struct {
	Kind string
	// TakesStaged is true when the kind takes the staged nodes.
	TakesStaged  bool
	LockUnpushed string
	Freshness    string
	// NotReady runs publish.not-ready.
	NotReady bool
	// NVR runs nvr.reused, nvr.removed and nvr.older.
	NVR      bool
	Advisory string
	// AcceptAdvisory is true when --accept-advisory can accept a block.
	AcceptAdvisory bool
	// Gate2 runs gate 2.
	Gate2       bool
	BaseSigners string
	Trust       string
	// SignsRPMs is true when step 2 can sign RPMs.
	SignsRPMs bool
	// Removes is true when the kind removes files of the base.
	Removes bool
}

// KindRules gives the rules of each kind.
var KindRules = map[string]Rules{
	KindPublish: {Kind: KindPublish, TakesStaged: true, LockUnpushed: LockAll, Freshness: FreshApplies, NotReady: true, NVR: true,
		Advisory: AdvisoryBlocks, AcceptAdvisory: true, Gate2: true, BaseSigners: SignersRefuse, Trust: TrustNewKeyfile, SignsRPMs: true},
	KindRestore: {Kind: KindRestore, LockUnpushed: LockSet, Freshness: FreshApplies,
		Advisory: AdvisoryBlocks, AcceptAdvisory: true, Gate2: true, BaseSigners: SignersResignRemove, Trust: TrustNewest, SignsRPMs: true, Removes: true},
	KindPrune: {Kind: KindPrune, LockUnpushed: LockSet, Freshness: FreshApplies,
		Advisory: AdvisoryClosure, Gate2: true, BaseSigners: SignersKept, Trust: TrustNever, Removes: true},
	KindResign: {Kind: KindResign, LockUnpushed: LockSet, Freshness: FreshApplies,
		Advisory: AdvisorySkip, Gate2: true, BaseSigners: SignersResignID, Trust: TrustNever, SignsRPMs: true},
	KindImport: {Kind: KindImport, LockUnpushed: LockSet, Freshness: FreshEmpty,
		Advisory: AdvisoryReport, Gate2: true, BaseSigners: SignersVerify, Trust: TrustNever},
	KindReseed: {Kind: KindReseed, LockUnpushed: LockSet, Freshness: FreshEmpty,
		Advisory: AdvisorySkip, Gate2: true, BaseSigners: SignersVerify, Trust: TrustNever},
}
