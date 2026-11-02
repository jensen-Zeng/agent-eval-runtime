package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	exportMaxResult          = 2 << 20
	exportMaxTrace           = 10 << 20
	exportMaxPatch           = 1 << 20
	exportMaxEval            = 2 << 20
	exportMaxProvenance      = 1 << 20
	exportMaxExperimentBytes = 512 << 20
)

var exportRunID = regexp.MustCompile(`^[0-9a-f]{24}$`)

func executeExport(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("runtime export", flag.ContinueOnError)
	fs.SetOutput(errout)
	runDir := fs.String("run", "", "local run artifact directory")
	output := fs.String("output", "", "new export directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(errout, "export:", err); return 1 }
	if fs.NArg() != 0 || strings.TrimSpace(*runDir) == "" || strings.TrimSpace(*output) == "" {
		return fail(fmt.Errorf("--run and --output are required"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	root, err := filepath.Abs(*runDir)
	if err != nil {
		return fail(err)
	}
	if err := requireRealExportDirectory(root); err != nil {
		return fail(fmt.Errorf("run artifact path: %w", err))
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fail(fmt.Errorf("run artifact path must be a real directory"))
	}
	resultBytes, err := exportReadRegular(root, "result.json", exportMaxResult)
	if err != nil {
		return fail(fmt.Errorf("read result.json: %w", err))
	}
	var report runner.Report
	dec := json.NewDecoder(strings.NewReader(string(resultBytes)))
	if err := dec.Decode(&report); err != nil {
		return fail(fmt.Errorf("decode result.json: %w", err))
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fail(fmt.Errorf("result.json must contain one JSON value"))
	}
	if report.SchemaVersion != 1 || !exportRunID.MatchString(report.RunID) || report.AttemptID != report.RunID+"-1" {
		return fail(fmt.Errorf("result.json has invalid schema or run/attempt identity"))
	}
	if !exportTerminalStatus(report.Status) || report.FinishedAt.IsZero() {
		return fail(fmt.Errorf("run report is incomplete"))
	}
	payloadKind, payloadHash, payloadLimit := "patch", report.PatchSHA256, int64(exportMaxPatch)
	exportName := "candidate.diff"
	if report.Backend == "harbor" {
		payloadKind = "submission"
		payloadHash = report.SubmissionSHA256
		payloadLimit = 72 << 20
		exportName = "submission.tar.gz"
	}
	patchPath, ok := report.Artifacts[payloadKind]
	if !ok {
		return fail(fmt.Errorf("run report is missing patch artifact"))
	}
	tracePath, ok := report.Artifacts["trace"]
	if !ok {
		return fail(fmt.Errorf("run report is missing trace artifact"))
	}
	patchBytes, err := exportReadRegular(root, patchPath, payloadLimit)
	if err != nil {
		return fail(fmt.Errorf("read patch artifact: %w", err))
	}
	patchHash := sha256.Sum256(patchBytes)
	if payloadHash == "" || hex.EncodeToString(patchHash[:]) != payloadHash {
		return fail(fmt.Errorf("patch SHA256 does not match run report"))
	}
	traceBytes, err := exportReadRegular(root, tracePath, exportMaxTrace)
	if err != nil {
		return fail(fmt.Errorf("read trace artifact: %w", err))
	}
	files := map[string]exportFile{
		"run.json":         {data: resultBytes, source: "result.json"},
		exportName:         {data: patchBytes, source: patchPath},
		"trajectory.jsonl": {data: traceBytes, source: tracePath},
	}
	if evalPath, exists := report.Artifacts["evaluation"]; exists {
		b, e := exportReadRegular(root, evalPath, exportMaxEval)
		if e != nil {
			return fail(fmt.Errorf("read evaluation artifact: %w", e))
		}
		files["evaluation.json"] = exportFile{data: b, source: evalPath}
	}
	if b, e := exportReadRegular(root, "provenance.json", exportMaxProvenance); e == nil {
		files["provenance.json"] = exportFile{data: b, source: "provenance.json"}
	} else if !errors.Is(e, os.ErrNotExist) {
		return fail(fmt.Errorf("read provenance.json: %w", e))
	}
	usage, err := json.MarshalIndent(map[string]any{
		"schema_version": 1, "run_id": report.RunID, "agent": report.Agent,
		"agent_config_sha256": report.AgentConfigSHA256, "usage": report.Usage,
	}, "", "  ")
	if err != nil {
		return fail(err)
	}
	files["AI_USAGE.json"] = exportFile{data: append(usage, '\n'), source: "result.json#/agent,agent_config_sha256,usage"}
	readme := []byte("# AgentEval run export\n\nThis bundle contains evidence copied from one completed runtime attempt. `trajectory.jsonl` preserves the recorded trace bytes; `candidate.diff` preserves the collected patch bytes. `AI_USAGE.json` records only the agent, configuration hash, and usage values present in the machine report. No reasoning, self-tests, or other events are fabricated. `provenance.json` is included only when a regular provenance file exists in the run directory. `BUNDLE_MANIFEST.json` hashes each exported payload except itself and records its source and the original result hash.\n")
	files["README.md"] = exportFile{data: readme, source: "generated explanation"}
	resultHash := sha256.Sum256(resultBytes)
	manifest := map[string]any{"schema_version": 1, "run_id": report.RunID, "result_sha256": hex.EncodeToString(resultHash[:]), "files": map[string]any{}}
	manifestFiles := manifest["files"].(map[string]any)
	for name, f := range files {
		h := sha256.Sum256(f.data)
		manifestFiles[name] = map[string]any{"sha256": hex.EncodeToString(h[:]), "size_bytes": len(f.data), "source": f.source}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fail(err)
	}
	files["BUNDLE_MANIFEST.json"] = exportFile{data: append(manifestBytes, '\n'), source: "generated manifest"}
	dest, err := filepath.Abs(*output)
	if err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fail(fmt.Errorf("output directory already exists"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("inspect output directory: %w", err))
	}
	parent := filepath.Dir(dest)
	if err := requireRealExportDirectory(parent); err != nil {
		return fail(fmt.Errorf("output parent: %w", err))
	}
	stage, err := os.MkdirTemp(parent, ".agenteval-export-")
	if err != nil {
		return fail(err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()
	for name, file := range files {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(filepath.Join(stage, name), file.data, 0600); err != nil {
			return fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fail(fmt.Errorf("output directory appeared during export"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err := os.Rename(stage, dest); err != nil {
		return fail(err)
	}
	published = true
	printJSON(out, map[string]any{"output": dest, "run_id": report.RunID, "file_count": len(files)})
	return 0
}

type experimentExportRecord struct {
	Index     int            `json:"index"`
	Case      experimentCase `json:"case"`
	JobID     string         `json:"job_id,omitempty"`
	JobStatus string         `json:"job_status,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	Status    string         `json:"status"`
	Bundle    string         `json:"bundle,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

type experimentExportIndex struct {
	SchemaVersion  int                      `json:"schema_version"`
	ExperimentID   string                   `json:"experiment_id"`
	ExperimentName string                   `json:"experiment_name"`
	ManifestSHA256 string                   `json:"manifest_sha256"`
	Records        []experimentExportRecord `json:"records"`
}

type experimentBundleState struct {
	path         string
	reason       string
	caseIdentity experimentCase
	snapshot     jobs.TaskSnapshot
}

// Execute a locally stored benchmark manifest into a reviewable bundle. It
// never follows report.ArtifactDir: run roots are derived solely from RunID.
func executeExperimentExport(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("dataset export", flag.ContinueOnError)
	fs.SetOutput(errout)
	manifestPath := fs.String("experiment", "", "frozen local experiment manifest")
	artifactsPath := fs.String("artifacts", "", "local run artifact root")
	output := fs.String("output", "", "new experiment export directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(errout, "dataset export:", err); return 1 }
	if fs.NArg() != 0 || *manifestPath == "" || *artifactsPath == "" || *output == "" {
		return fail(fmt.Errorf("--experiment, --artifacts and --output are required"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	manifestAbs, err := filepath.Abs(*manifestPath)
	if err != nil {
		return fail(err)
	}
	if err := requireRealExportDirectory(filepath.Dir(manifestAbs)); err != nil {
		return fail(fmt.Errorf("experiment manifest parent: %w", err))
	}
	mi, err := os.Lstat(manifestAbs)
	if err != nil {
		return fail(err)
	}
	if mi.Mode()&os.ModeSymlink != 0 || !mi.Mode().IsRegular() || mi.Size() < 0 || mi.Size() > maxExperimentManifestBytes {
		return fail(fmt.Errorf("experiment manifest must be regular and <=32 MiB"))
	}
	manifestBytes, err := exportReadSiblingFile(manifestAbs, maxExperimentManifestBytes)
	if err != nil {
		return fail(fmt.Errorf("read experiment manifest: %w", err))
	}
	var manifest experimentManifest
	d := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	d.DisallowUnknownFields()
	if err := d.Decode(&manifest); err != nil {
		return fail(fmt.Errorf("invalid experiment manifest: %w", err))
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fail(fmt.Errorf("experiment manifest must contain exactly one JSON value"))
	}
	if err := validateExportExperimentManifest(manifest); err != nil {
		return fail(err)
	}
	manifestHash := sha256.Sum256(manifestBytes)
	artifactsRoot, err := filepath.Abs(*artifactsPath)
	if err != nil {
		return fail(err)
	}
	if err := requireRealExportDirectory(artifactsRoot); err != nil {
		return fail(fmt.Errorf("artifact root: %w", err))
	}
	dest, err := filepath.Abs(*output)
	if err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fail(fmt.Errorf("output directory already exists"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	parent := filepath.Dir(dest)
	if err := requireRealExportDirectory(parent); err != nil {
		return fail(fmt.Errorf("output parent: %w", err))
	}
	for _, entry := range manifest.Entries {
		runID := entry.JobRunID()
		if runID == "" && entry.Stats != nil {
			runID = entry.Stats.RunID
		}
		if exportRunID.MatchString(runID) && exportPathWithin(filepath.Join(artifactsRoot, runID), dest) {
			return fail(fmt.Errorf("output must be outside every source run directory"))
		}
	}
	stage, err := os.MkdirTemp(parent, ".agenteval-experiment-export-")
	if err != nil {
		return fail(err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()
	index := experimentExportIndex{SchemaVersion: 1, ExperimentID: manifest.ExperimentID, ExperimentName: manifest.Name, ManifestSHA256: hex.EncodeToString(manifestHash[:]), Records: make([]experimentExportRecord, 0, len(manifest.Entries))}
	runs := map[string]experimentBundleState{}
	var totalBytes int64
	exported := 0
	for i, e := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		rec := experimentExportRecord{Index: i, Case: e.Case, Status: "omitted"}
		if e.Job != nil {
			rec.JobID = e.Job.ID
			rec.JobStatus = e.Job.Status
			rec.RunID = e.Job.RunID
		}
		if rec.RunID == "" && e.Stats != nil {
			rec.RunID = e.Stats.RunID
		}
		runID, reason := completedExperimentRunID(e)
		if reason != "" {
			rec.Reason = reason
			index.Records = append(index.Records, rec)
			continue
		}
		rec.RunID = runID
		if prior, ok := runs[runID]; ok {
			if prior.path != "" && sameExperimentExportIdentity(prior.caseIdentity, e.Case) && prior.snapshot == e.TaskSnapshot {
				rec.Status = "reused"
				rec.Bundle = prior.path
			} else if prior.path != "" {
				rec.Reason = "run_id is already associated with a different task, model, agent, or snapshot"
			} else {
				rec.Reason = prior.reason
			}
			index.Records = append(index.Records, rec)
			continue
		}
		bundleRel := runID
		bundlePath := filepath.Join(stage, runID)
		var stdout, stderr strings.Builder
		code := executeExport(ctx, []string{"--run", filepath.Join(artifactsRoot, runID), "--output", bundlePath}, &stdout, &stderr)
		if code != 0 {
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = "run evidence failed export validation"
			}
			runs[runID] = experimentBundleState{reason: reason}
			rec.Reason = reason
			index.Records = append(index.Records, rec)
			continue
		}
		if err := experimentRunMatchesEntry(filepath.Join(bundlePath, "run.json"), runID, e); err != nil {
			_ = os.RemoveAll(bundlePath)
			runs[runID] = experimentBundleState{reason: err.Error()}
			rec.Reason = err.Error()
			index.Records = append(index.Records, rec)
			continue
		}
		size, err := exportTreeSize(bundlePath)
		if err != nil {
			_ = os.RemoveAll(bundlePath)
			runs[runID] = experimentBundleState{reason: err.Error()}
			rec.Reason = err.Error()
			index.Records = append(index.Records, rec)
			continue
		}
		if totalBytes+size > exportMaxExperimentBytes {
			_ = os.RemoveAll(bundlePath)
			reason := "experiment export exceeds 512 MiB total bundle limit"
			runs[runID] = experimentBundleState{reason: reason}
			rec.Reason = reason
			index.Records = append(index.Records, rec)
			continue
		}
		totalBytes += size
		exported++
		identity := e.Case
		identity.Repeat = 0
		runs[runID] = experimentBundleState{path: bundleRel, caseIdentity: identity, snapshot: e.TaskSnapshot}
		rec.Status = "exported"
		rec.Bundle = bundleRel
		index.Records = append(index.Records, rec)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	omitted := 0
	for _, record := range index.Records {
		if record.Status == "omitted" {
			omitted++
		}
	}
	if exported == 0 {
		printJSON(out, map[string]any{"index": index, "run_bundles": 0, "omitted_count": omitted})
		fmt.Fprintln(errout, "dataset export: no run bundles passed validation")
		return 2
	}
	indexBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "index.json"), append(indexBytes, '\n'), 0600); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fail(fmt.Errorf("output directory appeared during export"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err := os.Rename(stage, dest); err != nil {
		return fail(err)
	}
	published = true
	printJSON(out, map[string]any{"output": dest, "experiment_id": manifest.ExperimentID, "run_bundles": exported, "entry_count": len(index.Records), "omitted_count": omitted, "bytes": totalBytes})
	if omitted > 0 {
		return 2
	}
	return 0
}

func (e experimentEntry) JobRunID() string {
	if e.Job == nil {
		return ""
	}
	return e.Job.RunID
}

func sameExperimentExportIdentity(a, b experimentCase) bool {
	a.Repeat = 0
	b.Repeat = 0
	return a == b
}

func experimentRunMatchesEntry(path, runID string, e experimentEntry) error {
	b, err := exportReadSiblingFile(path, exportMaxResult)
	if err != nil {
		return fmt.Errorf("read exported report: %w", err)
	}
	var r runner.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("invalid exported report: %w", err)
	}
	if r.RunID != runID || r.TaskID != e.Case.TaskID || r.TaskRevision != e.TaskSnapshot.TaskRevision || r.SourceSHA256 != e.TaskSnapshot.SourceSHA256 || r.EvaluatorSHA256 != e.TaskSnapshot.EvaluatorSHA256 || r.ImageID != e.TaskSnapshot.ImageID || r.AgentConfigSHA256 != e.TaskSnapshot.AgentConfigSHA256 || r.Agent != e.Case.Agent {
		return fmt.Errorf("run report identity or frozen task snapshot does not match experiment entry")
	}
	if e.TaskSnapshot.Backend == "harbor" && (r.Backend != "harbor" || r.PackageSHA256 != e.TaskSnapshot.PackageSHA256 || r.VerifierImageID != e.TaskSnapshot.VerifierImageID) {
		return fmt.Errorf("Harbor run package or verifier image differs from frozen experiment")
	}
	return nil
}

func validateExportExperimentManifest(m experimentManifest) error {
	if m.SchemaVersion != 1 || !experimentIDPattern.MatchString(m.ExperimentID) || !experimentNamePattern.MatchString(m.Name) || m.Name != m.Input.Name || len(m.Entries) == 0 || len(m.Entries) > maxExperimentJobs {
		return fmt.Errorf("invalid experiment identity or entries")
	}
	in := m.Input
	if err := validateExperiment(&in); err != nil {
		return fmt.Errorf("invalid frozen experiment input: %w", err)
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != m.InputSHA256 {
		return fmt.Errorf("experiment input hash mismatch")
	}
	want := experimentCases(in)
	if len(want) != len(m.Entries) {
		return fmt.Errorf("experiment case count does not match frozen input")
	}
	seenCases := map[experimentCase]bool{}
	seenJobs := map[string]bool{}
	for i, e := range m.Entries {
		if e.Case != want[i] || seenCases[e.Case] || e.IdempotencyKey != experimentIdempotencyKey(m.ExperimentID, e.Case) || !sameExperimentRequest(e.Request, requestForExperimentCase(e.Case)) || !validExperimentSnapshot(e.TaskSnapshot) {
			return fmt.Errorf("invalid or duplicate experiment case at entry %d", i)
		}
		seenCases[e.Case] = true
		if e.Job != nil {
			if e.Job.ID == "" || seenJobs[e.Job.ID] {
				return fmt.Errorf("duplicate or missing job ID at entry %d", i)
			}
			seenJobs[e.Job.ID] = true
			if e.Job.IdempotencyKey != e.IdempotencyKey || !sameExperimentRequest(e.Job.Request, e.Request) || e.Job.TaskSnapshot != e.TaskSnapshot {
				return fmt.Errorf("job does not match frozen request/snapshot at entry %d", i)
			}
		}
	}
	return nil
}

func completedExperimentRunID(e experimentEntry) (string, string) {
	if e.Job == nil {
		return "", "job was never submitted"
	}
	if e.Job.Status == jobs.StatusQueued || e.Job.Status == jobs.StatusRunning || e.Job.FinishedAt == nil {
		return "", "job is not complete"
	}
	switch e.Job.Status {
	case jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusTimedOut, jobs.StatusCanceled, jobs.StatusInfraError:
	default:
		return "", "job has an invalid terminal status"
	}
	jobID := e.Job.RunID
	statsID := ""
	if e.Stats != nil {
		statsID = e.Stats.RunID
	}
	if jobID != "" && statsID != "" && jobID != statsID {
		return "", "job run_id conflicts with stats run_id"
	}
	if !exportRunID.MatchString(jobID) {
		return "", "job has no valid 24-hex run_id"
	}
	if statsID != "" && !exportRunID.MatchString(statsID) {
		return "", "stats has an invalid run_id"
	}
	if e.Stats != nil && statsID == "" {
		return "", "stats is missing run_id"
	}
	return jobID, ""
}

func exportReadSiblingFile(path string, max int64) ([]byte, error) {
	parent := filepath.Dir(path)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > max || !os.SameFile(pathInfo, before) {
		return nil, fmt.Errorf("manifest changed or exceeds size limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || int64(len(b)) > max || int64(len(b)) != after.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("manifest changed while being read")
	}
	return b, nil
}

func exportPathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func exportTreeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("export bundle contains a link")
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("export bundle contains a special file")
		}
		total += info.Size()
		return nil
	})
	return total, err
}

type exportFile struct {
	data   []byte
	source string
}

func exportReadRegular(root, rel string, max int64) ([]byte, error) {
	if filepath.IsAbs(rel) || rel == "" || strings.ContainsAny(rel, `\:`) || filepath.ToSlash(filepath.Clean(rel)) != rel || rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
		return nil, fmt.Errorf("invalid relative artifact path")
	}
	parts := strings.Split(rel, "/")
	current := root
	var expectedInfo os.FileInfo
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid artifact path")
		}
		current = filepath.Join(current, filepath.FromSlash(part))
		st, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("artifact path contains a link")
		}
		if i < len(parts)-1 && !st.IsDir() {
			return nil, fmt.Errorf("artifact parent is not a directory")
		}
		if i == len(parts)-1 && (!st.Mode().IsRegular() || st.Size() < 0 || st.Size() > max) {
			return nil, fmt.Errorf("artifact must be regular and within size limit")
		}
		if i == len(parts)-1 {
			expectedInfo = st
		}
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rootHandle.Close()
	f, err := rootHandle.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > max || expectedInfo == nil || !os.SameFile(expectedInfo, before) {
		return nil, fmt.Errorf("artifact changed or is outside size limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("artifact exceeds size limit")
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(b)) != after.Size() {
		return nil, fmt.Errorf("artifact changed while being read")
	}
	return b, nil
}

func requireRealExportDirectory(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	volume := filepath.VolumeName(abs)
	separator := string(filepath.Separator)
	current := volume + separator
	rest := strings.TrimPrefix(abs, current)
	if volume == "" && filepath.IsAbs(abs) {
		current = separator
		rest = strings.TrimLeft(abs, separator)
	}
	if volume != "" && rest == abs {
		return fmt.Errorf("invalid directory path")
	}
	rootInfo, err := os.Lstat(current)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("directory root is a link or non-directory")
	}
	for _, component := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		st, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return fmt.Errorf("directory path contains a link or non-directory component")
		}
	}
	return nil
}

func exportTerminalStatus(status string) bool {
	switch status {
	case "SUCCEEDED", "FAILED", "TIMED_OUT", "CANCELED", "INFRA_ERROR":
		return true
	default:
		return false
	}
}
