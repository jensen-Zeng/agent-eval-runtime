package harbor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/pelletier/go-toml/v2"
)

func validFiles() map[string][]byte {
	return map[string][]byte{
		"task.toml": []byte(`version = "T3-RL-v1.0"
artifacts = ["/app"]
[topic]
L1 = "Agent"
L2 = "CodeAgent"
L3 = "VisualReasoning"
[metadata]
instance_id = "sample-task"
task = "demo"
source = "owned"
language = "zh"
prompt_creation_time = "2026-10-19"
[environment]
cpus = 4
memory_mb = 8192
storage_mb = 16384
workdir = "/app"
network_mode = "no-network"
skip_upload = false
[environment.upload]
source = "environment/workspace"
target = "/app"
[environment.env]
PYTHONUTF8 = "1"
[agent]
timeout_sec = 14400.0
[verifier]
environment_mode = "separate"
timeout_sec = 1800.0
[judge]
type = "programmatic"
reward_kind = "continuous"
[other]
grader = "tests/grade_submission.py"
required_outputs = ["run.py", "answer.json"]
[rollout_baseline.oracle]
reward = 1.0
[rollout_baseline.nop]
reward = 0.0
`),
		"instruction.md":                  []byte("请处理输入。"),
		"environment/Dockerfile":          []byte("FROM python:3.11-slim\nWORKDIR /app\nCOPY workspace/ /app/\n"),
		"environment/workspace/input.gif": []byte{0x47, 0x49, 0x46, 0x38},
		"environment/workspace/main.py":   []byte("print('ok')\n"),
		"tests/Dockerfile":                []byte("FROM python:3.11-slim\nWORKDIR /tests\nCOPY . /tests/\n"),
		"tests/test.sh":                   []byte("#!/bin/sh\npython3 /tests/grade_submission.py\nmkdir -p /logs/verifier\nprintf '{}\\n' > /logs/verifier/reward.json\n"),
		"tests/grade_submission.py":       []byte("print('grade')\n"),
		"tests/fixture/rubric.json":       []byte("{}\n"),
		"solution/solve.sh":               []byte("#!/bin/sh\nexit 0\n"),
	}
}

func writeFixture(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for name, b := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateT3RLAndSeparateBundles(t *testing.T) {
	root := t.TempDir()
	files := validFiles()
	writeFixture(t, root, files)
	s, got, err := ValidatePackage(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != Version || len(got) != len(files) {
		t.Fatalf("unexpected validated package: version=%q files=%d", s.Version, len(got))
	}
	workspace := subset(got, "environment/workspace/", true)
	tests := subset(got, "tests/", true)
	if _, leaked := workspace["../tests/grade_submission.py"]; leaked {
		t.Fatal("grader leaked into workspace")
	}
	if _, ok := tests["grade_submission.py"]; !ok {
		t.Fatal("grader missing from trusted verifier bundle")
	}
}

func TestRequiredOutputReservedDirectories(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "reserved metadata directory", path: "__pycache__/result.json", wantErr: true},
		{name: "ordinary cache directory", path: "cache/result.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := validFiles()
			manifest := strings.Replace(string(files["task.toml"]), `required_outputs = ["run.py", "answer.json"]`, `required_outputs = ["`+tc.path+`"]`, 1)
			files["task.toml"] = []byte(manifest)
			writeFixture(t, root, files)
			_, _, err := ValidatePackage(root)
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "reserved metadata directory")) {
				t.Fatalf("expected reserved-directory rejection, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ordinary cache output rejected: %v", err)
			}
		})
	}
}

func TestVerifierTmpfsBoundaries(t *testing.T) {
	if err := sandbox.ValidateWritableDirs([]string{"/stage_runs"}); err != nil {
		t.Fatalf("valid verifier scratch path rejected: %v", err)
	}
	for _, dirs := range [][]string{
		{"relative"},
		{"/stage_runs/nested"},
		{"/app"},
		{"/logs"},
		{"/stage_runs", "/stage_runs"},
		{"/a", "/b", "/c", "/d", "/e"},
	} {
		if err := sandbox.ValidateWritableDirs(dirs); err == nil {
			t.Errorf("accepted invalid verifier scratch paths %q", dirs)
		}
	}
}

func TestLoadChecksFrozenPackageAndHashes(t *testing.T) {
	project := t.TempDir()
	taskRoot := filepath.Join(project, "tasks", "harbor", "sample-task")
	packageRoot := filepath.Join(taskRoot, "package")
	files := validFiles()
	writeFixture(t, packageRoot, files)
	spec, err := decodeSpec(files["task.toml"])
	if err != nil {
		t.Fatal(err)
	}
	selected := selectedPackage(files)
	h := HashFiles(selected)
	task, err := newTask("sample-task", "package", h, spec, selected, 8192, "sha256:"+repeatHex('a'), "sha256:"+repeatHex('b'))
	if err != nil {
		t.Fatal(err)
	}
	task.Harbor.VerifierTmpfs = []string{"/stage_runs"}
	if err = os.MkdirAll(taskRoot, 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(task)
	manifest := filepath.Join(taskRoot, "task.json")
	if err = os.WriteFile(manifest, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(task, manifest, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Harbor.VerifierTmpfs) != 1 || loaded.Harbor.VerifierTmpfs[0] != "/stage_runs" {
		t.Fatalf("verifier tmpfs override was not preserved: %q", loaded.Harbor.VerifierTmpfs)
	}
	invalid := task
	invalid.Harbor = &domain.HarborSpec{}
	*invalid.Harbor = *task.Harbor
	invalid.Harbor.VerifierTmpfs = []string{"/app"}
	if _, err = Load(invalid, manifest, project); err == nil {
		t.Fatal("Loader accepted a verifier tmpfs that overlaps the system workspace")
	}
	if !loaded.IsHarbor() || string(loaded.Source["input.gif"]) != string([]byte{0x47, 0x49, 0x46, 0x38}) || loaded.Evaluator["grade_submission.py"] == nil {
		t.Fatal("Harbor bundles were not loaded as separate opaque inputs")
	}
	if err = os.WriteFile(filepath.Join(packageRoot, "tests", "grade_submission.py"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(task, manifest, project); err == nil {
		t.Fatal("accepted changed frozen package")
	}
}

func TestRejectUnsupportedHarborModes(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement, want string }{
		{"shared", `environment_mode = "separate"`, `environment_mode = "shared"`, "separate"},
		{"network", `network_mode = "no-network"`, `network_mode = "public"`, "network_mode"},
		{"prebuilt", `network_mode = "no-network"`, "network_mode = \"no-network\"\ndocker_image = \"internal/image:v1\"", "prebuilt-only"},
		{"judge", `type = "programmatic"`, `type = "llm"`, "programmatic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := validFiles()
			s := strings.Replace(string(files["task.toml"]), tc.old, tc.replacement, 1)
			files["task.toml"] = []byte(s)
			root := t.TempDir()
			writeFixture(t, root, files)
			_, _, err := ValidatePackage(root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected rejection containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestReadFilesRejectsCredentialAndLinkInputs(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string][]byte{"ok.txt": []byte("ok")})
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFiles(root, MaxPackageBytes, MaxPackageBytes, MaxPackageFiles); err == nil {
		t.Fatal("accepted .env file")
	}
}

func decodeSpec(b []byte) (taskTOML, error) {
	var s taskTOML
	err := toml.Unmarshal(b, &s)
	return s, err
}
func repeatHex(c byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = c
	}
	return string(out)
}
