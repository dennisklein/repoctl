package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/testkit"
)

func compile(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	data := File(name)
	if data == nil {
		t.Fatalf("no schema %s", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(ID(name), doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(ID(name))
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return s
}

// validateYAML converts YAML to JSON as an editor does and validates it.
func validateYAML(s *jsonschema.Schema, data []byte) error {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	return s.Validate(inst)
}

func TestSchemasAreJSON(t *testing.T) {
	for _, n := range Names() {
		var v map[string]any
		if err := json.Unmarshal(File(n), &v); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if v["$schema"] != "https://json-schema.org/draft/2020-12/schema" || v["$id"] != ID(n) {
			t.Errorf("%s: $schema %v, $id %v", n, v["$schema"], v["$id"])
		}
		compile(t, n)
	}
	if File("nope") != nil {
		t.Fatal("unknown name gives a schema")
	}
}

// story gives the story set without the fake host: only its files.
func story() *testkit.Story {
	s := &testkit.Story{Root: "/srv/story", Pkgs: testkit.StoryPackages(), Externals: map[string]string{}}
	for _, p := range s.Pkgs {
		if p.External {
			s.Externals[p.ID] = "/srv/story/ext/" + p.ID + ".git"
		}
	}
	return s
}

func TestStoryFilesValidate(t *testing.T) {
	s := story()
	cfg := []byte(s.SetYAML())
	if _, err := config.ParseSet(cfg, "repoctl.yaml"); err != nil {
		t.Fatalf("the story repoctl.yaml does not parse: %v", err)
	}
	if err := validateYAML(compile(t, "config"), cfg); err != nil {
		t.Fatalf("story repoctl.yaml: %v\n%s", err, cfg)
	}
	commits := map[string]string{}
	for _, p := range s.Pkgs {
		if p.Path == "" {
			commits[p.ID] = strings.Repeat("ab", 20)
		}
	}
	lk := []byte(s.LockText(commits))
	if _, err := lock.Parse(lk, "repoctl.lock"); err != nil {
		t.Fatal(err)
	}
	if err := validateYAML(compile(t, "lock"), lk); err != nil {
		t.Fatalf("story repoctl.lock: %v\n%s", err, lk)
	}
	// The lock that repoctl writes validates too.
	f, _ := lock.Parse(lk, "repoctl.lock")
	if err := validateYAML(compile(t, "lock"), f.Format(nil)); err != nil {
		t.Fatalf("formatted lock: %v", err)
	}
	if err := validateYAML(compile(t, "lock"), lock.Empty().Format(nil)); err != nil {
		t.Fatalf("empty lock: %v", err)
	}
}

func TestDesignExamplesValidate(t *testing.T) {
	for file, name := range map[string]string{
		"testdata/design-repoctl.yaml":     "config",
		"testdata/design-repoctl.lock":     "lock",
		"testdata/design-user-config.yaml": "user-config",
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateYAML(compile(t, name), data); err != nil {
			t.Errorf("%s: %v", file, err)
		}
	}
}

func TestSchemasRefuse(t *testing.T) {
	cfg := compile(t, "config")
	base := "apiVersion: repoctl/v1alpha1\nname: s\nrepo: {name: s}\n"
	for _, bad := range []string{
		base + "targets: []\n",
		base + "targets: [{id: el9, mock: rocky+epel-9-x86_64, plugins: [foo]}]\n",
		base + "targets: [{id: el9, mock: rocky+epel-9-aarch64, plugins: [rpmautospec]}]\n",
		base + "targets: [{id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec]}]\nunknown: 1\n",
		base + "targets: [{id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec]}]\npackages: [{id: a, branch: b, path: p}]\n",
		base + "targets: [{id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec]}]\npackages: [{id: a, path: p, git: {url: x}}]\n",
		base + "targets: [{id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec]}]\npackages: [{id: a, branch: b, role: release}]\n",
		strings.Replace(base, "name: s\n", "name: S!\n", 1) + "targets: [{id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec]}]\n",
	} {
		if err := validateYAML(cfg, []byte(bad)); err == nil {
			t.Errorf("config schema takes:\n%s", bad)
		}
	}
	lk := compile(t, "lock")
	for _, bad := range []string{
		"apiVersion: repoctl/v1alpha1\npackages: {a: {branch: b}}\n",
		"apiVersion: repoctl/v1alpha1\npackages: {a: {branch: b, commit: 123}}\n",
		"apiVersion: repoctl/v1alpha2\npackages: {}\n",
	} {
		if err := validateYAML(lk, []byte(bad)); err == nil {
			t.Errorf("lock schema takes:\n%s", bad)
		}
	}
	uc := compile(t, "user-config")
	for _, bad := range []string{
		"name: slurm\n",
		"jobs: 0\n",
		"sets: {slurm-26.05: {packages: []}}\n",
		"progress: fancy\n",
		"lock_wait: soon\n",
	} {
		if err := validateYAML(uc, []byte(bad)); err == nil {
			t.Errorf("user-config schema takes:\n%s", bad)
		}
	}
}

// TestUserConfigHasEachSetting keeps the schema next to config.Settings:
// each key has a property at its dotted path.
func TestUserConfigHasEachSetting(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(File("user-config"), &doc); err != nil {
		t.Fatal(err)
	}
	keys := doc["$defs"].(map[string]any)["Keys"].(map[string]any)
	for _, s := range config.Settings {
		node := keys
		for _, part := range strings.Split(s.Name, ".") {
			props, ok := node["properties"].(map[string]any)
			if !ok {
				t.Errorf("%s: no properties at %s", s.Name, part)
				break
			}
			next, ok := props[part].(map[string]any)
			if !ok {
				t.Errorf("the user-config schema has no %s", s.Name)
				break
			}
			node = next
		}
	}
	uc := compile(t, "user-config")
	if err := validateYAML(uc, []byte("jobs: 3\nlock_wait: 2h\nbuild: {timeout: {rpm: 6h, run: 0}}\nsets: {slurm-26.05: {jobs: 1, mock: {nosync: true}}}\n")); err != nil {
		t.Fatal(err)
	}
}
