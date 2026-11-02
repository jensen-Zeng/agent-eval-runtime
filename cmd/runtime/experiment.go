package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/client"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxExperimentJobs = 1000
const maxExperimentInputBytes = 2 << 20
const maxExperimentManifestBytes = 32 << 20

var experimentNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
var experimentTaskPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
var experimentIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var experimentHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var experimentImagePattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type experimentInput struct {
	SchemaVersion int                 `json:"schema_version"`
	Name          string              `json:"name"`
	Tasks         []string            `json:"tasks"`
	Variants      []experimentVariant `json:"variants"`
	Repeats       int                 `json:"repeats"`
}

type experimentVariant struct {
	Agent  string `json:"agent"`
	Script string `json:"script,omitempty"`
	Model  string `json:"model,omitempty"`
}

type experimentCase struct {
	TaskID string `json:"task_id"`
	Agent  string `json:"agent"`
	Script string `json:"script,omitempty"`
	Model  string `json:"model,omitempty"`
	Repeat int    `json:"repeat"`
}

type experimentEntry struct {
	Case           experimentCase      `json:"case"`
	Request        jobs.Request        `json:"request"`
	TaskSnapshot   jobs.TaskSnapshot   `json:"task_snapshot"`
	IdempotencyKey string              `json:"idempotency_key"`
	Reused         bool                `json:"reused,omitempty"`
	Job            *jobs.Job           `json:"job,omitempty"`
	Stats          *experimentRunStats `json:"stats,omitempty"`
	Error          string              `json:"error,omitempty"`
}

type experimentResource struct {
	CPU          *float64 `json:"cpu_seconds,omitempty"`
	Memory       *uint64  `json:"memory_peak_sampled_bytes,omitempty"`
	Samples      int      `json:"samples"`
	SampleErrors int      `json:"sample_errors"`
}

type experimentRunStats struct {
	RunID          string                        `json:"run_id,omitempty"`
	Verdict        string                        `json:"verdict,omitempty"`
	WallMS         *int64                        `json:"wall_ms,omitempty"`
	Usage          *domain.Usage                 `json:"usage,omitempty"`
	Score          *float64                      `json:"score,omitempty"`
	Classification string                        `json:"classification,omitempty"`
	Resources      map[string]experimentResource `json:"resources,omitempty"`
}

type experimentGroup struct {
	TaskID                 string         `json:"task_id"`
	Agent                  string         `json:"agent"`
	Script                 string         `json:"script,omitempty"`
	Model                  string         `json:"model,omitempty"`
	Total                  int            `json:"total"`
	Terminal               int            `json:"terminal"`
	Success                int            `json:"success"`
	Failure                int            `json:"failure"`
	InfraErrors            int            `json:"infra_errors"`
	Unknown                int            `json:"unknown"`
	SuccessRate            *float64       `json:"success_rate,omitempty"`
	ScoreAverage           *float64       `json:"score_average,omitempty"`
	ScoreCount             int            `json:"score_count"`
	ScoreUnknown           int            `json:"score_unknown"`
	Classifications        map[string]int `json:"classifications,omitempty"`
	TokenTotal             int64          `json:"token_total"`
	TokenEstimatedTotal    int64          `json:"token_estimated_total"`
	TokenEstimatedRuns     int            `json:"token_estimated_runs"`
	TokenUnknown           int            `json:"token_unknown"`
	WallMS                 int64          `json:"wall_ms"`
	WallUnknown            int            `json:"wall_unknown"`
	CPUSeconds             float64        `json:"cpu_seconds"`
	CPUUnknown             int            `json:"cpu_unknown"`
	MemoryPeakSampledBytes uint64         `json:"memory_peak_sampled_bytes"`
	MemoryUnknown          int            `json:"memory_unknown"`
	ResourceSamples        int64          `json:"resource_samples"`
	ResourceSampleErrors   int64          `json:"resource_sample_errors"`
}

type experimentSummary struct {
	Total       int               `json:"total"`
	Submitted   int               `json:"submitted"`
	Succeeded   int               `json:"succeeded"`
	Failed      int               `json:"failed"`
	InfraErrors int               `json:"infra_errors"`
	Unknown     int               `json:"unknown"`
	Groups      []experimentGroup `json:"groups"`
}

type experimentManifest struct {
	SchemaVersion int               `json:"schema_version"`
	ExperimentID  string            `json:"experiment_id"`
	Name          string            `json:"name"`
	InputSHA256   string            `json:"input_sha256"`
	Input         experimentInput   `json:"input"`
	StartedAt     time.Time         `json:"started_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Complete      bool              `json:"complete"`
	Entries       []experimentEntry `json:"entries"`
	Summary       experimentSummary `json:"summary"`
}

type experimentPreviewer interface {
	Preview(context.Context, jobs.Request) (jobs.Preview, error)
}

func executeExperiment(parent context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("runtime experiment", flag.ContinueOnError)
	fs.SetOutput(errout)
	urlFlag := fs.String("url", defaultQueueURL, "loopback runtime API URL")
	inputPath := fs.String("file", "examples/v1.0-experiment.json", "frozen experiment matrix JSON")
	outputPath := fs.String("output", "", "manifest path (defaults to artifacts/experiments/<name>.json)")
	csvPath := fs.String("csv", "", "optional grouped CSV summary path")
	wait := fs.Bool("wait", false, "wait for all submitted jobs to reach terminal state")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall experiment deadline")
	resume := fs.Bool("resume", false, "resume only the exact matrix recorded in the existing manifest")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 || *timeout <= 0 {
		fmt.Fprintln(errout, "experiment accepts no positional arguments; --timeout must be positive")
		return 1
	}
	input, inputHash, err := readExperiment(*inputPath)
	if err != nil {
		fmt.Fprintln(errout, "experiment:", err)
		return 1
	}
	if *outputPath == "" {
		*outputPath = filepath.Join("artifacts", "experiments", input.Name+".json")
	}
	if err := distinctOutputPaths(*inputPath, *outputPath, *csvPath); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	api, err := client.New(*urlFlag)
	if err != nil {
		fmt.Fprintln(errout, "URL:", err)
		return 1
	}
	defer api.Close()
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	code := executeExperimentMatrix(ctx, api, input, inputHash, *outputPath, *csvPath, *wait, *resume, out, errout)
	return code
}

func readExperiment(path string) (experimentInput, string, error) {
	var input experimentInput
	f, err := os.Open(path)
	if err != nil {
		return input, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxExperimentInputBytes+1))
	if err != nil {
		return input, "", err
	}
	if len(b) > maxExperimentInputBytes {
		return input, "", errors.New("input exceeds the 2 MiB limit")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return input, "", fmt.Errorf("invalid JSON: %w", err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return input, "", errors.New("input must contain exactly one JSON value")
	}
	if err := validateExperiment(&input); err != nil {
		return input, "", err
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return input, "", err
	}
	sum := sha256.Sum256(canonical)
	return input, hex.EncodeToString(sum[:]), nil
}

func validateExperiment(input *experimentInput) error {
	if input.SchemaVersion != 1 || !experimentNamePattern.MatchString(input.Name) {
		return errors.New("schema_version must be 1 and name must be a safe 1..80 character identifier")
	}
	if len(input.Tasks) == 0 || len(input.Tasks) > maxExperimentJobs || len(input.Variants) == 0 || len(input.Variants) > maxExperimentJobs || input.Repeats < 1 || input.Repeats > 100 {
		return errors.New("tasks/variants must be non-empty and repeats must be 1..100")
	}
	if len(input.Tasks)*len(input.Variants) > maxExperimentJobs/input.Repeats {
		return fmt.Errorf("experiment matrix exceeds the %d-job limit", maxExperimentJobs)
	}
	seenTasks := map[string]bool{}
	for i, id := range input.Tasks {
		if !experimentTaskPattern.MatchString(id) || seenTasks[id] {
			return fmt.Errorf("tasks[%d] is invalid or duplicated", i)
		}
		seenTasks[id] = true
	}
	seenVariants := map[string]bool{}
	for i := range input.Variants {
		v := &input.Variants[i]
		if v.Agent == "" {
			v.Agent = "scripted"
		}
		if v.Agent != "scripted" && v.Agent != "llm" || len(v.Script) > 80 || len(v.Model) > 256 || strings.TrimSpace(v.Script) != v.Script || strings.ContainsAny(v.Script, "\r\n\x00") || strings.TrimSpace(v.Model) != v.Model || strings.ContainsAny(v.Model, "\r\n\x00") || (v.Agent == "llm" && (v.Script != "" || v.Model == "")) || (v.Agent == "scripted" && (v.Model != "" || v.Script == "")) {
			return fmt.Errorf("variants[%d] agent/script/model fields are incompatible", i)
		}
		key, _ := json.Marshal(v)
		if seenVariants[string(key)] {
			return fmt.Errorf("variants[%d] is duplicated", i)
		}
		seenVariants[string(key)] = true
	}
	return nil
}

func experimentCases(input experimentInput) []experimentCase {
	out := make([]experimentCase, 0, len(input.Tasks)*len(input.Variants)*input.Repeats)
	for _, taskID := range input.Tasks {
		for _, variant := range input.Variants {
			for n := 1; n <= input.Repeats; n++ {
				out = append(out, experimentCase{TaskID: taskID, Agent: variant.Agent, Script: variant.Script, Model: variant.Model, Repeat: n})
			}
		}
	}
	return out
}

func requestForExperimentCase(c experimentCase) jobs.Request {
	return jobs.Request{SchemaVersion: 1, TaskID: c.TaskID, Agent: c.Agent, Script: c.Script, Model: c.Model}
}

func sameExperimentRequest(a, b jobs.Request) bool {
	return a.SchemaVersion == b.SchemaVersion && a.TaskID == b.TaskID && a.Agent == b.Agent && a.Script == b.Script && a.Model == b.Model
}

func caseKey(c experimentCase) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func experimentIdempotencyKey(id string, c experimentCase) string {
	return "exp-" + id + "-" + caseKey(c)[:24]
}

func experimentID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func executeExperimentMatrix(ctx context.Context, api *client.Client, input experimentInput, inputHash, outputPath, csvPath string, wait, resume bool, out, errout io.Writer) int {
	previewer, ok := any(api).(experimentPreviewer)
	if !ok {
		fmt.Fprintln(errout, "runtime API does not support frozen task preview; upgrade the service before starting an experiment")
		return 1
	}
	manifest, err := loadOrCreateExperiment(outputPath, input, inputHash, resume)
	if err != nil {
		fmt.Fprintln(errout, "manifest:", err)
		return 1
	}
	// Freeze each task/variant once before creating any jobs.
	previews := map[string]struct {
		req      jobs.Request
		snapshot jobs.TaskSnapshot
	}{}
	for _, c := range experimentCases(input) {
		key := string(mustJSON([]any{c.TaskID, c.Agent, c.Script, c.Model}))
		if _, found := previews[key]; found {
			continue
		}
		preview, e := previewer.Preview(ctx, requestForExperimentCase(c))
		if e != nil {
			fmt.Fprintln(errout, "preview:", e)
			return 1
		}
		if !sameExperimentRequest(preview.Request, requestForExperimentCase(c)) || !validExperimentSnapshot(preview.TaskSnapshot) {
			fmt.Fprintln(errout, "preview returned a non-canonical request or incomplete frozen snapshot")
			return 1
		}
		previews[key] = struct {
			req      jobs.Request
			snapshot jobs.TaskSnapshot
		}{preview.Request, preview.TaskSnapshot}
	}
	if resume {
		for i := range manifest.Entries {
			c := manifest.Entries[i].Case
			p := previews[string(mustJSON([]any{c.TaskID, c.Agent, c.Script, c.Model}))]
			if !sameExperimentRequest(manifest.Entries[i].Request, p.req) || manifest.Entries[i].TaskSnapshot != p.snapshot {
				fmt.Fprintf(errout, "resume rejected: frozen task/agent snapshot changed for %s repeat %d\n", c.TaskID, c.Repeat)
				return 1
			}
		}
		// The local file is a checkpoint, not an authority for job results. Re-submit
		// every stable idempotency key to rehydrate current server truth before summary.
		for i := range manifest.Entries {
			manifest.Entries[i].Job = nil
			manifest.Entries[i].Stats = nil
			manifest.Entries[i].Error = ""
			manifest.Entries[i].Reused = false
		}
	} else {
		id, e := experimentID()
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		manifest.ExperimentID = id
		manifest.Entries = make([]experimentEntry, 0, len(experimentCases(input)))
		for _, c := range experimentCases(input) {
			p := previews[string(mustJSON([]any{c.TaskID, c.Agent, c.Script, c.Model}))]
			manifest.Entries = append(manifest.Entries, experimentEntry{Case: c, Request: p.req, TaskSnapshot: p.snapshot, IdempotencyKey: experimentIdempotencyKey(id, c)})
		}
		manifest.StartedAt = time.Now().UTC()
	}
	if err := writeExperimentManifest(outputPath, &manifest); err != nil {
		fmt.Fprintln(errout, "manifest:", err)
		return 1
	}

	for i := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			break
		}
		entry := &manifest.Entries[i]
		request := entry.Request
		frozen := entry.TaskSnapshot
		request.ExpectedSnapshot = &frozen
		var job jobs.Job
		var reused bool
		var submitErr error
		for {
			job, reused, submitErr = api.Submit(ctx, request, entry.IdempotencyKey)
			if submitErr == nil || !wait {
				break
			}
			var apiErr *client.APIError
			if !errors.As(submitErr, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
				break
			}
			delay := apiErr.RetryAfter
			if delay <= 0 {
				delay = time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				submitErr = ctx.Err()
			case <-timer.C:
			}
			if ctx.Err() != nil {
				break
			}
		}
		if submitErr != nil {
			entry.Error = submitErr.Error()
		} else if job.TaskSnapshot != entry.TaskSnapshot {
			entry.Error = "submitted job snapshot differs from frozen preview; job is not included as a valid experiment run"
			entry.Job = &job
			fmt.Fprintln(errout, "fatal: submitted task snapshot changed after preview; stopping before more jobs")
			_ = writeExperimentManifest(outputPath, &manifest)
			break
		} else {
			entry.Job, entry.Reused, entry.Error = &job, reused, ""
			entry.Stats = nil
			compactExperimentEntry(entry)
		}
		if err := writeExperimentManifest(outputPath, &manifest); err != nil {
			fmt.Fprintln(errout, "manifest:", err)
			printJSON(out, manifest)
			return 1
		}
	}
	if wait && ctx.Err() == nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for hasExperimentActive(manifest.Entries) {
			select {
			case <-ctx.Done():
				goto finishedExperiment
			case <-ticker.C:
				refreshExperimentJobs(ctx, api, manifest.Entries)
				if err := writeExperimentManifest(outputPath, &manifest); err != nil {
					fmt.Fprintln(errout, "manifest:", err)
					printJSON(out, manifest)
					return 1
				}
			}
		}
	}
finishedExperiment:
	manifest.Summary = summarizeExperiment(manifest.Entries)
	manifest.Complete = manifest.Summary.Submitted == manifest.Summary.Total && manifest.Summary.Unknown == 0
	if err := writeExperimentManifest(outputPath, &manifest); err != nil {
		fmt.Fprintln(errout, "manifest:", err)
		printJSON(out, manifest)
		return 1
	}
	if csvPath != "" {
		if err := writeExperimentCSV(csvPath, manifest.Summary.Groups); err != nil {
			fmt.Fprintln(errout, "CSV:", err)
			printJSON(out, manifest)
			return 1
		}
	}
	printJSON(out, manifest)
	fmt.Fprintf(errout, "experiment %s saved to %s\n", manifest.ExperimentID, outputPath)
	if ctx.Err() != nil {
		fmt.Fprintf(errout, "experiment stopped: %v; resume with the same input and --resume\n", ctx.Err())
		return 1
	}
	for _, e := range manifest.Entries {
		if e.Error != "" {
			return 1
		}
	}
	if !wait {
		return 0
	}
	if manifest.Summary.Unknown > 0 || manifest.Summary.InfraErrors > 0 {
		return 1
	}
	if manifest.Summary.Failed > 0 {
		return 2
	}
	return 0
}

func loadOrCreateExperiment(path string, input experimentInput, inputHash string, resume bool) (experimentManifest, error) {
	var manifest experimentManifest
	info, err := os.Lstat(path)
	if resume {
		if err != nil {
			return manifest, fmt.Errorf("--resume requires an existing manifest: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return manifest, errors.New("resume manifest must be a regular non-symlink file")
		}
		f, e := os.Open(path)
		if e != nil {
			return manifest, e
		}
		defer f.Close()
		data, e := io.ReadAll(io.LimitReader(f, maxExperimentManifestBytes+1))
		if e != nil {
			return manifest, e
		}
		if len(data) > maxExperimentManifestBytes {
			return manifest, errors.New("resume manifest exceeds 32 MiB")
		}
		d := json.NewDecoder(strings.NewReader(string(data)))
		d.DisallowUnknownFields()
		if e = d.Decode(&manifest); e != nil {
			return manifest, e
		}
		var extra any
		if e = d.Decode(&extra); e != io.EOF {
			return manifest, errors.New("resume manifest must contain one JSON value")
		}
		frozenInput, freezeErr := manifest.Input, error(nil)
		if freezeErr == nil {
			freezeErr = validateExperiment(&frozenInput)
		}
		frozenBytes, marshalErr := json.Marshal(frozenInput)
		frozenHash := sha256.Sum256(frozenBytes)
		if manifest.SchemaVersion != 1 || manifest.Name != input.Name || manifest.InputSHA256 != inputHash || freezeErr != nil || marshalErr != nil || hex.EncodeToString(frozenHash[:]) != inputHash || len(manifest.Entries) != len(experimentCases(input)) || !experimentIDPattern.MatchString(manifest.ExperimentID) {
			return manifest, errors.New("resume input differs from frozen experiment manifest or manifest is invalid")
		}
		want := experimentCases(input)
		for i, c := range want {
			e := manifest.Entries[i]
			if e.Case != c || e.IdempotencyKey != experimentIdempotencyKey(manifest.ExperimentID, c) || !sameExperimentRequest(e.Request, requestForExperimentCase(c)) || !validExperimentSnapshot(e.TaskSnapshot) {
				return manifest, fmt.Errorf("resume entry %d is invalid", i)
			}
		}
		return manifest, nil
	}
	if err == nil {
		return manifest, errors.New("output manifest already exists; use --resume with the identical input, or select a new output path for a new experiment")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return manifest, err
	}
	return experimentManifest{SchemaVersion: 1, Name: input.Name, InputSHA256: inputHash, Input: input, Entries: []experimentEntry{}, Summary: experimentSummary{Groups: []experimentGroup{}}}, nil
}

func validExperimentSnapshot(s jobs.TaskSnapshot) bool {
	if s.Backend == "harbor" {
		if !experimentHashPattern.MatchString(s.PackageSHA256) || !experimentImagePattern.MatchString(s.VerifierImageID) {
			return false
		}
	} else if s.Backend != "" && s.Backend != "go" {
		return false
	}
	return s.TaskRevision > 0 && experimentHashPattern.MatchString(s.ManifestSHA256) && experimentHashPattern.MatchString(s.SourceSHA256) && experimentHashPattern.MatchString(s.EvaluatorSHA256) && experimentImagePattern.MatchString(s.ImageID) && experimentHashPattern.MatchString(s.AgentConfigSHA256)
}

func writeExperimentManifest(path string, m *experimentManifest) error {
	m.UpdatedAt = time.Now().UTC()
	for i := range m.Entries {
		compactExperimentEntry(&m.Entries[i])
	}
	m.Summary = summarizeExperiment(m.Entries)
	m.Complete = m.Summary.Submitted == m.Summary.Total && m.Summary.Unknown == 0 && m.Summary.InfraErrors == 0
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if st, e := os.Lstat(path); e == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return errors.New("manifest target must be a regular non-symlink file")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	tmp, err := os.CreateTemp(dir, ".experiment-*.tmp")
	if err != nil {
		return err
	}
	name, ok := tmp.Name(), false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("atomic manifest replacement failed (previous manifest retained): %w", err)
	}
	ok = true
	return nil
}

func hasExperimentActive(entries []experimentEntry) bool {
	for _, e := range entries {
		if e.Job == nil || isActive(e.Job.Status) {
			if e.Error == "" {
				return true
			}
		}
	}
	return false
}

func refreshExperimentJobs(ctx context.Context, api *client.Client, entries []experimentEntry) {
	type result struct {
		index int
		job   jobs.Job
		err   error
	}
	indices := make([]int, 0)
	for i, e := range entries {
		if e.Job != nil && isActive(e.Job.Status) {
			indices = append(indices, i)
		}
	}
	jobsCh := make(chan int)
	results := make(chan result, len(indices))
	workers := min(8, len(indices))
	if workers == 0 {
		return
	}
	var wg sync.WaitGroup
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobsCh {
				job, err := api.Get(ctx, entries[i].Job.ID)
				results <- result{i, job, err}
			}
		}()
	}
	go func() {
		defer close(jobsCh)
		for _, i := range indices {
			select {
			case <-ctx.Done():
				return
			case jobsCh <- i:
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	for r := range results {
		if r.err != nil {
			entries[r.index].Error = r.err.Error()
		} else {
			entries[r.index].Job, entries[r.index].Error = &r.job, ""
			compactExperimentEntry(&entries[r.index])
		}
	}
}

// Results can contain large evaluator output. Persist only the benchmark fields
// needed for summaries; full logs remain in the per-run artifact directory.
func compactExperimentEntry(e *experimentEntry) {
	if e.Job == nil || len(e.Job.Result) == 0 {
		return
	}
	if e.Stats == nil {
		var raw struct {
			RunID      string        `json:"run_id"`
			Verdict    string        `json:"verdict"`
			WallMS     *int64        `json:"wall_ms"`
			Usage      *domain.Usage `json:"usage"`
			Evaluation *struct {
				Score          *float64 `json:"score"`
				Classification string   `json:"classification"`
			} `json:"evaluation"`
			Resources map[string]experimentResource `json:"resources"`
		}
		if json.Unmarshal(e.Job.Result, &raw) == nil {
			e.Stats = &experimentRunStats{RunID: raw.RunID, Verdict: raw.Verdict, WallMS: raw.WallMS, Usage: raw.Usage, Resources: raw.Resources}
			if raw.Evaluation != nil {
				e.Stats.Score = raw.Evaluation.Score
				e.Stats.Classification = raw.Evaluation.Classification
			}
		}
	}
	job := *e.Job
	job.Result = nil
	e.Job = &job
}

func summarizeExperiment(entries []experimentEntry) experimentSummary {
	type groupKey struct{ task, agent, script, model string }
	groups := map[groupKey]*experimentGroup{}
	s := experimentSummary{Total: len(entries)}
	for _, e := range entries {
		k := groupKey{e.Case.TaskID, e.Case.Agent, e.Case.Script, e.Case.Model}
		g := groups[k]
		if g == nil {
			g = &experimentGroup{TaskID: k.task, Agent: k.agent, Script: k.script, Model: k.model}
			groups[k] = g
		}
		g.Total++
		if e.Job == nil {
			g.Unknown++
			s.Unknown++
			g.TokenUnknown++
			g.WallUnknown++
			g.CPUUnknown++
			g.MemoryUnknown++
			g.ScoreUnknown++
			continue
		}
		s.Submitted++
		if isActive(e.Job.Status) {
			g.Unknown++
			s.Unknown++
			g.TokenUnknown++
			g.WallUnknown++
			g.CPUUnknown++
			g.MemoryUnknown++
			g.ScoreUnknown++
			continue
		}
		if e.Stats == nil && len(e.Job.Result) > 0 {
			compactExperimentEntry(&e)
		}
		g.Terminal++
		if e.Job.Status == jobs.StatusSucceeded {
			g.Success++
			s.Succeeded++
		} else {
			g.Failure++
			s.Failed++
			if e.Job.Status == jobs.StatusInfraError || (e.Stats != nil && e.Stats.Classification == "infrastructure_failure") {
				g.InfraErrors++
				s.InfraErrors++
			}
		}
		if e.Stats == nil && len(e.Job.Result) > 0 {
			compactExperimentEntry(&e)
		}
		report := e.Stats
		if report == nil {
			g.TokenUnknown++
			g.WallUnknown++
			g.CPUUnknown++
			g.MemoryUnknown++
			g.ScoreUnknown++
			continue
		}
		if report.WallMS != nil && report.RunID != "" {
			g.WallMS += *report.WallMS
		} else {
			g.WallUnknown++
		}
		if report.Score == nil {
			g.ScoreUnknown++
		} else {
			current := 0.0
			if g.ScoreAverage != nil {
				current = *g.ScoreAverage * float64(g.ScoreCount)
			}
			g.ScoreCount++
			current += *report.Score
			average := current / float64(g.ScoreCount)
			g.ScoreAverage = &average
		}
		if report.Classification != "" {
			if g.Classifications == nil {
				g.Classifications = map[string]int{}
			}
			g.Classifications[report.Classification]++
		}
		if report.Usage == nil || (report.Usage.Quality != "known" && report.Usage.Quality != "reported" && report.Usage.Quality != "estimated") {
			g.TokenUnknown++
		} else if report.Usage.Quality == "estimated" {
			g.TokenEstimatedTotal += int64(report.Usage.TotalTokens)
			g.TokenEstimatedRuns++
		} else {
			g.TokenTotal += int64(report.Usage.TotalTokens)
		}
		cpuComplete, memComplete := len(report.Resources) > 0, len(report.Resources) > 0
		for _, r := range report.Resources {
			g.ResourceSamples += int64(r.Samples)
			g.ResourceSampleErrors += int64(r.SampleErrors)
			if r.CPU != nil {
				g.CPUSeconds += *r.CPU
			} else {
				cpuComplete = false
			}
			if r.Memory != nil {
				if *r.Memory > g.MemoryPeakSampledBytes {
					g.MemoryPeakSampledBytes = *r.Memory
				}
			} else {
				memComplete = false
			}
		}
		if !cpuComplete {
			g.CPUUnknown++
		}
		if !memComplete {
			g.MemoryUnknown++
		}
	}
	for _, g := range groups {
		if evaluated := g.Terminal - g.InfraErrors; evaluated > 0 {
			rate := float64(g.Success) / float64(evaluated)
			g.SuccessRate = &rate
		}
		s.Groups = append(s.Groups, *g)
	}
	sort.Slice(s.Groups, func(i, j int) bool {
		a, b := s.Groups[i], s.Groups[j]
		if a.TaskID != b.TaskID {
			return a.TaskID < b.TaskID
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Script != b.Script {
			return a.Script < b.Script
		}
		return false
	})
	return s
}

func writeExperimentCSV(path string, groups []experimentGroup) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return errors.New("CSV target must be a regular non-symlink file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".experiment-*.csv.tmp")
	if err != nil {
		return err
	}
	name, ok := tmp.Name(), false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err = tmp.Chmod(0600); err != nil {
		return err
	}
	w := csv.NewWriter(tmp)
	if err = w.Write([]string{"task_id", "agent", "script", "model", "total", "terminal", "success", "failure", "infra_errors", "unknown", "success_rate", "score_average", "score_count", "score_unknown", "token_total", "token_estimated_total", "token_estimated_runs", "token_unknown", "wall_ms", "wall_unknown", "cpu_seconds", "cpu_unknown", "memory_peak_sampled_bytes", "memory_unknown"}); err != nil {
		return err
	}
	for _, g := range groups {
		rate := ""
		if g.SuccessRate != nil {
			rate = fmt.Sprintf("%.6f", *g.SuccessRate)
		}
		score := ""
		if g.ScoreAverage != nil {
			score = fmt.Sprintf("%.6f", *g.ScoreAverage)
		}
		row := []string{g.TaskID, g.Agent, g.Script, g.Model, fmt.Sprint(g.Total), fmt.Sprint(g.Terminal), fmt.Sprint(g.Success), fmt.Sprint(g.Failure), fmt.Sprint(g.InfraErrors), fmt.Sprint(g.Unknown), rate, score, fmt.Sprint(g.ScoreCount), fmt.Sprint(g.ScoreUnknown), fmt.Sprint(g.TokenTotal), fmt.Sprint(g.TokenEstimatedTotal), fmt.Sprint(g.TokenEstimatedRuns), fmt.Sprint(g.TokenUnknown), fmt.Sprint(g.WallMS), fmt.Sprint(g.WallUnknown), fmt.Sprintf("%.6f", g.CPUSeconds), fmt.Sprint(g.CPUUnknown), fmt.Sprint(g.MemoryPeakSampledBytes), fmt.Sprint(g.MemoryUnknown)}
		for i := 0; i < 4; i++ {
			row[i] = safeCSVCell(row[i])
		}
		if err = w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	if err = w.Error(); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func safeCSVCell(s string) string {
	if len(s) > 0 && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@' || s[0] == '\t' || s[0] == '\r') {
		return "'" + s
	}
	return s
}

func distinctOutputPaths(input, output, csvPath string) error {
	paths := []string{input, output}
	if csvPath != "" {
		paths = append(paths, csvPath)
	}
	abs := make([]string, len(paths))
	for i, p := range paths {
		v, e := filepath.Abs(p)
		if e != nil {
			return e
		}
		abs[i] = filepath.Clean(v)
	}
	for i := 0; i < len(abs); i++ {
		for j := i + 1; j < len(abs); j++ {
			if strings.EqualFold(abs[i], abs[j]) {
				return errors.New("input, manifest, and CSV paths must be different")
			}
			a, e1 := os.Stat(abs[i])
			b, e2 := os.Stat(abs[j])
			if e1 == nil && e2 == nil && os.SameFile(a, b) {
				return errors.New("input, manifest, and CSV paths must refer to different files")
			}
		}
	}
	return nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
