package workspace

import "testing"

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://Git.GSI.de/SDE/packages/rocm-smi.git": "https://git.gsi.de/SDE/packages/rocm-smi",
		"https://git.gsi.de/SDE/packages/rocm-smi/":    "https://git.gsi.de/SDE/packages/rocm-smi",
		"git@github.com:GSI-HPC/slurm-singularity.git": "ssh://git@github.com/GSI-HPC/slurm-singularity",
	} {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	if CloneHash("https://git.gsi.de/SDE/packages/rocm-smi.git") != CloneHash("https://GIT.gsi.de/SDE/packages/rocm-smi") {
		t.Fatal("two spellings give two clones")
	}
}
