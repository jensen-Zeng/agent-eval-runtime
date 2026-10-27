package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/dataset"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"
)

// Dataset adapters only normalize inputs. Execution, scoring and repeated
// experiments continue through the same task registry and runtime contracts.
func executeDataset(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(out, "Commands: dataset scan, import, matrix, image, export\nUse dataset <command> --help for flags.")
		return 0
	}
	switch args[0] {
	case "scan":
		return executeDatasetScan(ctx, args[1:], out, errout)
	case "import":
		return executeDatasetImport(ctx, args[1:], out, errout)
	case "matrix":
		return executeDatasetMatrix(args[1:], out, errout)
	case "image":
		return executeDatasetImage(ctx, args[1:], out, errout)
	case "export":
		return executeExperimentExport(ctx, args[1:], out, errout)
	default:
		fmt.Fprintln(errout, "unknown dataset command")
		return 1
	}
}

func executeDatasetScan(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("dataset scan", flag.ContinueOnError)
	fs.SetOutput(errout)
	root := fs.String("root", "", "operator-owned local dataset root")
	adapter := fs.String("adapter", "e3", "directory adapter")
	output := fs.String("output", "", "optional new catalogue JSON file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if *root == "" || fs.NArg() != 0 {
		fmt.Fprintln(errout, "scan requires --root")
		return 1
	}
	c, err := dataset.Scan(ctx, *root, *adapter)
	if err != nil {
		fmt.Fprintln(errout, "scan:", err)
		return 1
	}
	if *output != "" {
		if err := writeNewJSON(*output, c); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	}
	printJSON(out, c)
	return 0
}

type datasetImportResult struct {
	Key             string `json:"key"`
	TaskID          string `json:"task_id"`
	UpstreamTaskID  string `json:"upstream_task_id"`
	Status          string `json:"status"`
	Manifest        string `json:"manifest,omitempty"`
	SourceSHA256    string `json:"source_sha256,omitempty"`
	EvaluatorSHA256 string `json:"evaluator_sha256,omitempty"`
	Reason          string `json:"reason,omitempty"`
}
type datasetImportReport struct {
	SchemaVersion   int                   `json:"schema_version"`
	Dataset         string                `json:"dataset"`
	Adapter         string                `json:"adapter"`
	CatalogSHA256   string                `json:"catalog_sha256"`
	CreatedAt       time.Time             `json:"created_at"`
	DryRun          bool                  `json:"dry_run"`
	RestartRequired bool                  `json:"restart_required"`
	Results         []datasetImportResult `json:"results"`
}

// Selection is explicit. A short ID that refers to more than one historical
// package is rejected; the stable catalogue key chooses the reviewed version.
func selectDatasetItems(c dataset.Catalog, selection string, prefix string) ([]dataset.Item, error) {
	if strings.TrimSpace(selection) == "" {
		return nil, fmt.Errorf("--select requires exact catalogue keys or unambiguous task IDs")
	}
	selected := []dataset.Item{}
	seen := map[string]bool{}
	seenIDs := map[string]bool{}
	for _, sel := range strings.Split(selection, ",") {
		sel = strings.TrimSpace(sel)
		matches := []dataset.Item{}
		for _, item := range c.Items {
			if item.Key == sel {
				matches = []dataset.Item{item}
				break
			}
			if item.TaskID == sel {
				matches = append(matches, item)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("selection %q matches %d items; choose a catalogue key", sel, len(matches))
		}
		item := matches[0]
		if seen[item.Key] {
			return nil, fmt.Errorf("duplicate selection %q", sel)
		}
		id := prefix + item.TaskID
		if !experimentTaskPattern.MatchString(id) || seenIDs[id] {
			return nil, fmt.Errorf("invalid or colliding destination task ID %q", id)
		}
		seen[item.Key] = true
		seenIDs[id] = true
		selected = append(selected, item)
	}
	return selected, nil
}

func executeDatasetImport(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("dataset import", flag.ContinueOnError)
	fs.SetOutput(errout)
	file := fs.String("file", "", "reviewed dataset catalogue JSON")
	project := fs.String("project", ".", "trusted runtime project root")
	selection := fs.String("select", "", "comma-separated catalogue keys or unambiguous task IDs")
	prefix := fs.String("prefix", "", "destination task ID prefix")
	output := fs.String("output", "", "optional new import report file")
	dry := fs.Bool("dry-run", false, "validate selected tasks without registering")
	existing := fs.String("if-exists", "fail", "fail or verify identical existing tasks")
	profile := fs.String("profile", "", "optional bounded runtime budget/limits profile")
	moduleCache := fs.String("module-cache", "", "image-owned offline module cache under /opt")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 || *file == "" {
		fmt.Fprintln(errout, "import requires --file and --select")
		return 1
	}
	if *existing != "fail" && *existing != "verify" {
		fmt.Fprintln(errout, "--if-exists must be fail or verify")
		return 1
	}
	if err := newOutputAvailable(*output); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	c, err := dataset.Load(*file)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	items, err := selectDatasetItems(c, *selection, *prefix)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	canonical, _ := json.Marshal(c)
	sum := sha256.Sum256(canonical)
	report := datasetImportReport{SchemaVersion: 1, Dataset: c.Name, Adapter: c.Adapter, CatalogSHA256: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC(), DryRun: *dry, Results: []datasetImportResult{}}
	blocked := false
	for _, item := range items {
		r := datasetImportResult{Key: item.Key, TaskID: *prefix + item.TaskID, UpstreamTaskID: item.TaskID, Status: "blocked"}
		if err := ctx.Err(); err != nil {
			r.Reason = err.Error()
			blocked = true
			report.Results = append(report.Results, r)
			break
		}
		if item.State != "candidate" && item.State != "ready" {
			r.Reason = "catalogue state " + item.State + " requires operator review"
			blocked = true
			report.Results = append(report.Results, r)
			continue
		}
		r, err = importDatasetItem(ctx, c, item, r, *project, *profile, *moduleCache, *existing, *dry)
		if err != nil {
			r.Status = "blocked"
			r.Reason = err.Error()
			blocked = true
		}
		if r.Status == "imported" {
			report.RestartRequired = true
		}
		report.Results = append(report.Results, r)
	}
	if *output != "" {
		if err := writeNewJSON(*output, report); err != nil {
			fmt.Fprintln(errout, err)
			printJSON(out, report)
			return 1
		}
	}
	printJSON(out, report)
	if blocked {
		return 2
	}
	return 0
}

func importDatasetItem(ctx context.Context, c dataset.Catalog, item dataset.Item, r datasetImportResult, project, profile, moduleCache, existing string, dry bool) (datasetImportResult, error) {
	if item.GraderDir == "" {
		return r, fmt.Errorf("trusted grader missing; ordinary repository tests cannot be reported as task success")
	}
	work, err := os.MkdirTemp("", "agenteval-dataset-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(work)
	if err := dataset.VerifyInputs(ctx, c.Root, item); err != nil {
		return r, err
	}
	source := filepath.Join(work, "source")
	if err := dataset.MaterializeSource(ctx, c.Root, item, source); err != nil {
		return r, err
	}
	prompt, err := dataset.ReadPrompts(ctx, c.Root, item)
	if err != nil {
		return r, err
	}
	if err := os.WriteFile(filepath.Join(work, "prompt.md"), []byte(prompt), 0600); err != nil {
		return r, err
	}
	grader, err := dataset.Resolve(c.Root, item.GraderDir)
	if err != nil {
		return r, err
	}
	// Copy and verify selected trusted grading bytes before invoking the generic
	// importer; it never reads a mutable answer-directory sibling.
	grading, err := task.ReadBundle(grader)
	if err != nil {
		return r, err
	}
	// A scanned grader has a pinned file set as well as pinned bytes. Reject
	// newly inserted helper files instead of silently expanding that snapshot.
	pinnedGrader := false
	for name := range item.ExpectedSHA256 {
		if strings.HasPrefix(name, item.GraderDir+"/") {
			pinnedGrader = true
			break
		}
	}
	if pinnedGrader {
		for name := range grading {
			if item.ExpectedSHA256[item.GraderDir+"/"+name] == "" {
				return r, fmt.Errorf("unrecognized grader file since scan: %s", name)
			}
		}
	}
	privateGrader := filepath.Join(work, "grader")
	if err := os.Mkdir(privateGrader, 0700); err != nil {
		return r, err
	}
	for name, data := range grading {
		hash := sha256.Sum256(data)
		expected := item.ExpectedSHA256[item.GraderDir+"/"+name]
		if expected != "" && !strings.EqualFold(expected, hex.EncodeToString(hash[:])) {
			return r, fmt.Errorf("grader input changed: %s", name)
		}
		dest := filepath.Join(privateGrader, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return r, err
		}
		if err := os.WriteFile(dest, data, 0600); err != nil {
			return r, err
		}
	}
	grader = privateGrader
	metadata := map[string]string{}
	for k, v := range item.Metadata {
		metadata[k] = v
	}
	metadata["dataset"] = c.Name
	metadata["adapter"] = c.Adapter
	metadata["dataset_key"] = item.Key
	metadata["upstream_task_id"] = item.TaskID
	metadata["package_path"] = item.PackagePath
	metadata["source_path"] = item.Source
	if item.SourceMember != "" {
		metadata["source_member"] = item.SourceMember
	}
	if len(item.SourceExcludes) > 0 {
		b, _ := json.Marshal(item.SourceExcludes)
		metadata["source_excludes"] = string(b)
	}
	// Pin all explicit public instructions as well as the imported content.
	promptHash := sha256.Sum256([]byte(prompt))
	metadata["instruction_sha256"] = hex.EncodeToString(promptHash[:])
	if err := runner.WriteJSON(filepath.Join(work, "metadata.json"), metadata); err != nil {
		return r, err
	}
	cmd := []string{"--project", project, "--id", r.TaskID, "--source", source, "--prompt", filepath.Join(work, "prompt.md"), "--grader-dir", grader, "--grader-entry", item.GraderEntry, "--metadata", filepath.Join(work, "metadata.json"), "--if-exists", existing, "--module-cache", moduleCache}
	if len(item.RequiredTests) > 0 {
		cmd = append(cmd, "--required-tests", strings.Join(item.RequiredTests, ","))
	}
	if len(item.AllowedFiles) > 0 {
		cmd = append(cmd, "--allowed-files", strings.Join(item.AllowedFiles, ","))
	}
	if profile != "" {
		cmd = append(cmd, "--profile", profile)
	}
	if dry {
		cmd = append(cmd, "--dry-run")
	}
	var stdout, stderr bytes.Buffer
	if code := executeImport(ctx, cmd, &stdout, &stderr); code != 0 {
		return r, fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
	}
	var normalized struct {
		Status          string `json:"status"`
		Manifest        string `json:"manifest"`
		SourceSHA256    string `json:"source_sha256"`
		EvaluatorSHA256 string `json:"evaluator_sha256"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &normalized); err != nil {
		return r, err
	}
	r.Status = normalized.Status
	r.Manifest = normalized.Manifest
	r.SourceSHA256 = normalized.SourceSHA256
	r.EvaluatorSHA256 = normalized.EvaluatorSHA256
	return r, nil
}

func executeDatasetMatrix(args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("dataset matrix", flag.ContinueOnError)
	fs.SetOutput(errout)
	reportFile := fs.String("report", "", "successful dataset import report JSON")
	output := fs.String("output", "", "new experiment matrix JSON")
	name := fs.String("name", "dataset-repeat", "experiment name")
	models := fs.String("models", "", "comma-separated model names; empty creates baseline-only matrix")
	repeats := fs.Int("repeats", 3, "independent repetitions per task and variant")
	baseline := fs.Bool("baseline", false, "include baseline alongside models")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 || *reportFile == "" || *output == "" {
		fmt.Fprintln(errout, "matrix requires --report and --output")
		return 1
	}
	var report datasetImportReport
	f, err := os.Open(*reportFile)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	err = task.DecodeStrict(f, &report)
	f.Close()
	if err != nil || report.SchemaVersion != 1 || report.DryRun || len(report.Results) == 0 || len(report.Results) > 1000 {
		fmt.Fprintln(errout, "matrix needs an actual schema 1 import report, not dry-run")
		return 1
	}
	input := experimentInput{SchemaVersion: 1, Name: *name, Repeats: *repeats}
	for _, r := range report.Results {
		if r.Status == "imported" || r.Status == "verified_existing" {
			input.Tasks = append(input.Tasks, r.TaskID)
		}
	}
	sort.Strings(input.Tasks)
	if *models != "" {
		for _, m := range strings.Split(*models, ",") {
			input.Variants = append(input.Variants, experimentVariant{Agent: "llm", Model: strings.TrimSpace(m)})
		}
	}
	if *models == "" || *baseline {
		input.Variants = append(input.Variants, experimentVariant{Agent: "scripted", Script: "baseline"})
	}
	if err := validateExperiment(&input); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if err := writeNewJSON(*output, input); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	printJSON(out, map[string]any{"schema_version": 1, "matrix": *output, "jobs": len(input.Tasks) * len(input.Variants) * input.Repeats, "input": input})
	return 0
}

// image emits a reviewable dependency-only build context. It does not execute
// repository code, download dependencies or expose private graders/answers.
func executeDatasetImage(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("dataset image", flag.ContinueOnError)
	fs.SetOutput(errout)
	file := fs.String("file", "", "reviewed dataset catalogue")
	selectIDs := fs.String("select", "", "catalogue keys or unambiguous task IDs")
	output := fs.String("output", "", "new dependency image build context directory")
	base := fs.String("base-image", "agenteval-sandbox:v1.0", "trusted local image tag or repository@sha256 digest")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 || *file == "" || *output == "" || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:-]{0,255}$`).MatchString(*base) {
		fmt.Fprintln(errout, "image requires --file, --select, --output and a safe base image")
		return 1
	}
	if strings.HasPrefix(*base, "sha256:") {
		fmt.Fprintln(errout, "Dockerfile FROM requires an image tag or repository@sha256 digest; a bare local image ID is unsupported")
		return 1
	}
	c, err := dataset.Load(*file)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	items, err := selectDatasetItems(c, *selectIDs, "")
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	abs, err := filepath.Abs(*output)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if _, err := os.Lstat(abs); !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(errout, "output already exists or is inaccessible")
		return 1
	}
	if err := requireRealExportDirectory(filepath.Dir(abs)); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	stage, err := os.MkdirTemp(filepath.Dir(abs), ".dataset-image-")
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer os.RemoveAll(stage)
	manifests := []map[string]any{}
	for _, item := range items {
		if item.State != "candidate" && item.State != "ready" {
			fmt.Fprintln(errout, "image selection requires reviewed candidate inputs")
			return 1
		}
		tmp, err := os.MkdirTemp("", "agenteval-modules-")
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		source := filepath.Join(tmp, "source")
		err = dataset.MaterializeSource(ctx, c.Root, item, source)
		if err != nil {
			os.RemoveAll(tmp)
			fmt.Fprintln(errout, err)
			return 1
		}
		files, err := task.ReadBundle(source)
		os.RemoveAll(tmp)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		mod, ok := files["go.mod"]
		if !ok {
			fmt.Fprintln(errout, "dependency image currently requires Go modules")
			return 1
		}
		dir := filepath.Join(stage, "modules", item.TaskID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), mod, 0600); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if sum, ok := files["go.sum"]; ok {
			if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0600); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		manifests = append(manifests, map[string]any{"task_id": item.TaskID, "catalogue_key": item.Key, "source_sha256": task.HashFiles(files)})
	}
	dockerfile := fmt.Sprintf("FROM %s\nUSER root\nCOPY modules /opt/agenteval/module-inputs\nRUN set -eu; mkdir -p /opt/agenteval/go-mod; for d in /opt/agenteval/module-inputs/*; do cd \"$d\"; GOTOOLCHAIN=local GOMODCACHE=/opt/agenteval/go-mod GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org go mod download; done; chmod -R a+rX /opt/agenteval/go-mod; rm -rf /opt/agenteval/module-inputs\nENV GOMODCACHE=/opt/agenteval/go-mod GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local\nUSER 10001:10001\n", *base)
	if err := os.WriteFile(filepath.Join(stage, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if err := runner.WriteJSON(filepath.Join(stage, "inputs.json"), map[string]any{"schema_version": 1, "base_image": *base, "tasks": manifests, "cache_path": "/opt/agenteval/go-mod"}); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if _, err := os.Lstat(abs); !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(errout, "output appeared during staging")
		return 1
	}
	if err := os.Rename(stage, abs); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	printJSON(out, map[string]any{"schema_version": 1, "build_context": abs, "tasks": len(items), "cache_path": "/opt/agenteval/go-mod", "build_required": true})
	return 0
}

func newOutputAvailable(name string) error {
	if name == "" {
		return nil
	}
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("output already exists or inaccessible: %s", name)
	}
	return requireRealExportDirectory(filepath.Dir(name))
}
func writeNewJSON(name string, value any) error {
	if err := newOutputAvailable(name); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}
