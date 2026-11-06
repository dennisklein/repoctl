package publish

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// TestMirrorScriptAgrees checks that contrib/mirror/mirror reads what
// repoctl pushes: the media types, the annotations, the apiVersions of the
// RepoManifest and the file names of the served tree.
func TestMirrorScriptAgrees(t *testing.T) {
	path := filepath.Join("..", "..", "contrib", "mirror", "mirror")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	value := func(name string) string {
		m := regexp.MustCompile(`(?m)^readonly ` + name + `=(.*)$`).FindStringSubmatch(script)
		if m == nil {
			t.Fatalf("the script has no readonly %s", name)
		}
		return m[1]
	}
	for name, want := range map[string]string{
		"MANIFEST_MEDIA_TYPE": ocispec.MediaTypeImageManifest,
		"ARTIFACT_TYPE":       snapshot.ArtifactType,
		"FILE_MEDIA_TYPE":     snapshot.FileMediaType,
		"ANN_TITLE":           snapshot.AnnTitle,
		"ANN_SET":             snapshot.AnnSet,
		"ANN_TARGET":          snapshot.AnnTarget,
		"ANN_PUBLISH":         snapshot.AnnPublish,
		"SNAPSHOT_FILE":       tree.ServedSnapshotFile,
		"READ_API_VERSIONS":   "(" + strings.Join(api.ReadAPIVersions, " ") + ")",
	} {
		if got := value(name); got != want {
			t.Errorf("%s=%s, want %s", name, got, want)
		}
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Errorf("bash -n: %v\n%s", err, out)
	}
	if _, err := exec.LookPath("shellcheck"); err == nil {
		if out, err := exec.Command("shellcheck", path).CombinedOutput(); err != nil {
			t.Errorf("shellcheck: %v\n%s", err, out)
		}
	}
}
