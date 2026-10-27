package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

func executeImport(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("runtime import", flag.ContinueOnError)
	fs.SetOutput(errout)
	root := fs.String("project", ".", "trusted project root")
	id := fs.String("id", "", "new task ID")
	source := fs.String("source", "", "source-only directory, zip or tar archive")
	prompt := fs.String("prompt", "", "UTF-8 prompt file")
	grader := fs.String("grader-dir", "", "explicit trusted grader directory (never the whole answer directory)")
	entry := fs.String("grader-entry", "grade.py", "Python grader entrypoint; blank for Go tests")
	required := fs.String("required-tests", "", "comma-separated required Go test names")
	allowed := fs.String("allowed-files", "", "comma-separated production Go files; default all existing production Go files")
	dryRun := fs.Bool("dry-run", false, "validate inputs and hashes without registering a task")
	ifExists := fs.String("if-exists", "fail", "fail or verify identical existing task")
	metadataFile := fs.String("metadata", "", "operator-owned provenance JSON object")
	profileFile := fs.String("profile", "", "trusted JSON budget/limits profile within local policy")
	moduleCache := fs.String("module-cache", "", "image-owned offline Go module cache under /opt")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(errout, "import:", err); return 1 }
	if fs.NArg() != 0 || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`).MatchString(*id) || *source == "" || *prompt == "" || *grader == "" {
		return fail(fmt.Errorf("--id, --source, --prompt and --grader-dir are required"))
	}
	project, err := filepath.Abs(*root)
	if err != nil {
		return fail(err)
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return fail(err)
	}
	parent := filepath.Join(project, "tasks", "fixtures")
	st, err := os.Lstat(parent)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fail(fmt.Errorf("task registry must be a real directory"))
	}
	if *ifExists != "fail" && *ifExists != "verify" {
		return fail(fmt.Errorf("--if-exists must be fail or verify"))
	}
	metadata := map[string]string(nil)
	if *metadataFile != "" {
		if err := readImportJSON(*metadataFile, &metadata); err != nil {
			return fail(err)
		}
	}
	profile := importProfile{Budget: domain.DefaultBudget(), Limits: sandbox.Limits{CPUs: 1, MemoryMiB: 1024, Pids: 64, WorkspaceMiB: 128}}
	profile.Budget.RunSeconds = 900
	profile.Budget.EvalSeconds = 300
	profile.Budget.MaxSteps = 50
	if *profileFile != "" {
		if err := readImportJSON(*profileFile, &profile); err != nil {
			return fail(err)
		}
	}
	destination := filepath.Join(parent, *id)
	existing := false
	if st, err := os.Lstat(destination); err == nil {
		if *ifExists != "verify" || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fail(fmt.Errorf("destination already exists or inaccessible"))
		}
		existing = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	promptInfo, err := os.Lstat(*prompt)
	if err != nil || !promptInfo.Mode().IsRegular() {
		return fail(fmt.Errorf("prompt must be a regular UTF-8 file"))
	}
	promptFile, err := os.Open(*prompt)
	if err != nil {
		return fail(err)
	}
	p, err := io.ReadAll(io.LimitReader(promptFile, 32769))
	closeErr := promptFile.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil || len(p) > 32768 || !utf8.Valid(p) {
		return fail(fmt.Errorf("prompt must be readable and <=32 KiB"))
	}
	// Only explicitly selected grading scripts/tests are imported; oracle patches
	// and answer metadata in sibling directories never enter a source sandbox.
	eval, err := task.ReadBundle(*grader)
	if err != nil {
		return fail(err)
	}
	for n := range eval {
		if !strings.HasSuffix(n, ".py") && !strings.HasSuffix(n, "_test.go") {
			return fail(fmt.Errorf("grader directory contains non-grading material %q", n))
		}
	}
	stage, err := os.MkdirTemp(parent, ".import-"+*id+"-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(stage)
	if err := task.ImportSource(ctx, *source, filepath.Join(stage, "source")); err != nil {
		return fail(err)
	}
	files, err := task.ReadBundle(filepath.Join(stage, "source"))
	if err != nil {
		return fail(err)
	}
	if err := os.Mkdir(filepath.Join(stage, "evaluator"), 0700); err != nil {
		return fail(err)
	}
	for n, b := range eval {
		f := filepath.Join(stage, "evaluator", filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(f), 0700); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(f, b, 0600); err != nil {
			return fail(err)
		}
	}
	split := func(s string) []string {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		var a []string
		for _, n := range strings.Split(s, ",") {
			a = append(a, strings.TrimSpace(n))
		}
		return a
	}
	paths := split(*allowed)
	if len(paths) == 0 {
		for n := range files {
			if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				paths = append(paths, n)
			}
		}
		sort.Strings(paths)
	}
	t := domain.Task{SchemaVersion: 1, ID: *id, Revision: 1, Metadata: metadata, Environment: domain.ExecutionEnvironment{GoModuleCache: *moduleCache}, Prompt: string(p), SourceDir: "source", SourceSHA256: task.HashFiles(files), EvaluatorDir: "evaluator", EvaluatorSHA256: task.HashFiles(eval), AllowedFiles: paths, RequiredTests: split(*required), Budget: profile.Budget, Limits: profile.Limits, Scripts: map[string]domain.Script{"baseline": {Actions: []domain.Action{{Tool: "finish", Arguments: json.RawMessage(`{"summary":"Baseline without code modifications"}`)}}}}}
	if *entry != "" {
		t.Grader = &domain.Grader{Kind: "python", EntryPoint: *entry}
	}
	if err := runner.WriteJSON(filepath.Join(stage, "task.json"), t); err != nil {
		return fail(err)
	}
	if _, err := task.Load(filepath.Join(stage, "task.json"), project); err != nil {
		return fail(err)
	}
	// A task package is data, never a package in the runtime module. The
	// nested module marker keeps trusted grader tests out of go test ./....
	if err := os.WriteFile(filepath.Join(stage, "go.mod"), []byte("module agenteval.local/benchmarks/"+*id+"\n\ngo 1.27.1\n"), 0600); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	status := "imported"
	if existing {
		old, err := task.Load(filepath.Join(destination, "task.json"), project)
		if err != nil {
			return fail(fmt.Errorf("existing task invalid: %w", err))
		}
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(t)
		if string(a) != string(b) {
			return fail(fmt.Errorf("existing task differs; use a new task ID or reviewed revision"))
		}
		status = "verified_existing"
	}
	if *dryRun || existing {
		if *dryRun {
			status = "validated"
		}
		printJSON(out, map[string]any{"schema_version": 1, "task_id": *id, "status": status, "manifest": filepath.Join(destination, "task.json"), "source_sha256": t.SourceSHA256, "evaluator_sha256": t.EvaluatorSHA256, "restart_required": false})
		return 0
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("destination appeared during import"))
	}
	if err := os.Rename(stage, destination); err != nil {
		return fail(err)
	}
	printJSON(out, map[string]any{"schema_version": 1, "task_id": *id, "status": "imported", "manifest": filepath.Join(destination, "task.json"), "source_sha256": t.SourceSHA256, "evaluator_sha256": t.EvaluatorSHA256, "restart_required": true})
	return 0
}

type importProfile struct {
	Budget domain.Budget  `json:"budget"`
	Limits sandbox.Limits `json:"limits"`
}

func readImportJSON(name string, out any) error {
	st, err := os.Lstat(name)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 32768 {
		return fmt.Errorf("profile/metadata must be a regular JSON file <=32 KiB")
	}
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return task.DecodeStrict(io.LimitReader(f, 32769), out)
}
