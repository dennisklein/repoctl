package api

// NewItem gives a new value of the item type of a kind, for decoding.
func NewItem(k Kind) any {
	switch k {
	case KindStatus:
		return new(Status)
	case KindPackage:
		return new(Package)
	case KindLock:
		return new(LockEntry)
	case KindGraph:
		return new(Graph)
	case KindPlan:
		return new(Plan)
	case KindRun:
		return new(Run)
	case KindNodeLog:
		return new(NodeLog)
	case KindRepoContents:
		return new(RepoContents)
	case KindRepoDiff:
		return new(RepoDiff)
	case KindProvenance:
		return new(Provenance)
	case KindSBOM:
		return new(Sbom)
	case KindSnapshot:
		return new(Snapshot)
	case KindSnapshotList:
		return new(SnapshotList)
	case KindAdvisory:
		return new(Advisory)
	case KindAdvisoryList:
		return new(AdvisoryList)
	case KindSignerList:
		return new(SignerList)
	case KindOnboardReport:
		return new(OnboardReport)
	case KindDoctorReport:
		return new(DoctorReport)
	case KindUpstream:
		return new(Upstream)
	case KindFile:
		return new(File)
	case KindClean:
		return new(Clean)
	case KindConfig:
		return new(Config)
	case KindPath:
		return new(Path)
	case KindSkill:
		return new(Skill)
	case KindVersion:
		return new(Version)
	}
	return nil
}
