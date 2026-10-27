package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/client"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

const (
	guiExperimentMaxCount  = 500
	guiExperimentMaxActive = 2
	guiRecordMaxBytes      = 256 << 10
	guiCSVMaxBytes         = 8 << 20
	guiOutputMaxBytes      = 64 << 10
	guiRunTimeout          = 30 * time.Minute
)

var guiExperimentIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

// guiDraft is deliberately limited to experiment choices. Provider secrets,
// filesystem paths and resource overrides are not accepted from browser input.
type guiDraft struct {
	Title    string   `json:"title"`
	Tasks    []string `json:"tasks"`
	Models   []string `json:"models"`
	Repeats  int      `json:"repeats"`
	Baseline bool     `json:"baseline"`
}

type guiExperimentRecord struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Draft     guiDraft  `json:"draft"`
	Exported  int       `json:"exported,omitempty"`
	Omitted   int       `json:"omitted,omitempty"`
}

type guiExperimentDetail struct {
	Record      guiExperimentRecord `json:"record"`
	ManifestRaw json.RawMessage     `json:"manifest,omitempty"`
}

type guiExperimentExport struct {
	Path     string `json:"path"`
	Exported int    `json:"exported"`
	Omitted  int    `json:"omitted"`
}

type guiExperimentManager struct {
	root   string
	apiURL string
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	closed      bool
	records     map[string]guiExperimentRecord
	active      map[string]context.CancelFunc
	cancelled   map[string]bool
	activeSlots chan struct{}
	exportGate  chan struct{}
	wg          sync.WaitGroup
}

func newGUIExperimentManager(ctx context.Context, rootArtifacts, apiURL string) (*guiExperimentManager, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(rootArtifacts) == "" {
		return nil, errors.New("artifacts root is required")
	}
	api, err := client.New(apiURL)
	if err != nil {
		return nil, fmt.Errorf("API URL: %w", err)
	}
	api.Close()
	root, err := filepath.Abs(rootArtifacts)
	if err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	if err := guiEnsureRealDir(root); err != nil {
		return nil, fmt.Errorf("artifacts root: %w", err)
	}
	for _, rel := range []string{"ui", filepath.Join("ui", "experiments")} {
		if err := guiEnsureRealDir(filepath.Join(root, rel)); err != nil {
			return nil, fmt.Errorf("GUI experiment directory: %w", err)
		}
	}
	managerCtx, cancel := context.WithCancel(ctx)
	m := &guiExperimentManager{
		root: filepath.Join(root, "ui", "experiments"), apiURL: apiURL,
		ctx: managerCtx, cancel: cancel,
		records: make(map[string]guiExperimentRecord), active: make(map[string]context.CancelFunc),
		cancelled: make(map[string]bool), activeSlots: make(chan struct{}, guiExperimentMaxActive),
		exportGate: make(chan struct{}, 1),
	}
	if err := m.loadRecords(); err != nil {
		cancel()
		return nil, err
	}
	return m, nil
}

func (m *guiExperimentManager) Start(ctx context.Context, draft guiDraft, requestID string) (guiExperimentRecord, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return guiExperimentRecord{}, err
		}
	}
	if !guiExperimentIDPattern.MatchString(requestID) {
		return guiExperimentRecord{}, errors.New("request ID must be 24 lowercase hexadecimal characters")
	}
	draft, input, err := guiNormalizeDraft(draft, requestID)
	if err != nil {
		return guiExperimentRecord{}, err
	}
	inputBytes, err := json.MarshalIndent(input, "", "  ")
	if err != nil || len(inputBytes) > maxExperimentInputBytes {
		return guiExperimentRecord{}, errors.New("experiment input exceeds the 2 MiB limit")
	}
	inputPath, manifestPath, csvPath, _, err := m.paths(requestID)
	if err != nil {
		return guiExperimentRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return guiExperimentRecord{}, errors.New("GUI experiment manager is closed")
	}
	if existing, ok := m.records[requestID]; ok {
		if !guiSameDraft(existing.Draft, draft) {
			return guiExperimentRecord{}, errors.New("request ID already belongs to a different experiment draft")
		}
		return existing, nil
	}
	if len(m.records) >= guiExperimentMaxCount {
		return guiExperimentRecord{}, errors.New("GUI experiment storage limit reached (500)")
	}
	select {
	case m.activeSlots <- struct{}{}:
	default:
		return guiExperimentRecord{}, errors.New("at most two GUI experiments may run at once")
	}
	if err := guiEnsureRealDir(filepath.Join(m.root, requestID)); err != nil {
		<-m.activeSlots
		return guiExperimentRecord{}, err
	}
	if err := guiAtomicWrite(m.root, filepath.Join(requestID, "input.json"), append(inputBytes, '\n'), maxExperimentInputBytes); err != nil {
		<-m.activeSlots
		return guiExperimentRecord{}, err
	}
	now := time.Now().UTC()
	record := guiExperimentRecord{ID: requestID, Title: draft.Title, State: "running", CreatedAt: now, UpdatedAt: now, Draft: draft}
	if err := m.saveRecordLocked(record); err != nil {
		<-m.activeSlots
		return guiExperimentRecord{}, err
	}
	runCtx, runCancel := context.WithTimeout(m.ctx, guiRunTimeout)
	m.active[requestID] = runCancel
	m.cancelled[requestID] = false
	m.wg.Add(1)
	go m.run(runCtx, requestID, inputPath, manifestPath, csvPath, false)
	return record, nil
}

func (m *guiExperimentManager) List() ([]guiExperimentRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshRecordsLocked(); err != nil {
		return nil, err
	}
	out := make([]guiExperimentRecord, 0, len(m.records))
	for _, r := range m.records {
		out = append(out, r)
	}
	sortGUIRecords(out)
	return out, nil
}

func (m *guiExperimentManager) HasRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, record := range m.records {
		if record.State == "running" {
			return true
		}
	}
	return false
}

func (m *guiExperimentManager) Detail(id string) (guiExperimentDetail, error) {
	if !guiExperimentIDPattern.MatchString(id) {
		return guiExperimentDetail{}, jobs.ErrNotFound
	}
	m.mu.Lock()
	record, ok := m.records[id]
	m.mu.Unlock()
	if !ok {
		return guiExperimentDetail{}, jobs.ErrNotFound
	}
	_, _, _, dir, err := m.paths(id)
	if err != nil {
		return guiExperimentDetail{}, err
	}
	var raw json.RawMessage
	if b, err := guiReadRegular(dir, "manifest.json", maxExperimentManifestBytes); err == nil {
		if !json.Valid(b) {
			return guiExperimentDetail{}, errors.New("experiment manifest is corrupt")
		}
		raw = append(raw, b...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return guiExperimentDetail{}, err
	}
	return guiExperimentDetail{Record: record, ManifestRaw: raw}, nil
}

func (m *guiExperimentManager) Resume(id string) (guiExperimentRecord, error) {
	if !guiExperimentIDPattern.MatchString(id) {
		return guiExperimentRecord{}, jobs.ErrNotFound
	}
	inputPath, manifestPath, csvPath, dir, err := m.paths(id)
	if err != nil {
		return guiExperimentRecord{}, err
	}
	if _, _, err := readExperiment(inputPath); err != nil {
		return guiExperimentRecord{}, fmt.Errorf("saved experiment input: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return guiExperimentRecord{}, errors.New("GUI experiment manager is closed")
	}
	record, ok := m.records[id]
	if !ok {
		return guiExperimentRecord{}, jobs.ErrNotFound
	}
	if record.State == "running" {
		return guiExperimentRecord{}, errors.New("experiment is already running")
	}
	if record.State == "completed" || record.State == "canceled" {
		return guiExperimentRecord{}, errors.New("completed or canceled experiments cannot be resumed")
	}
	for _, file := range []struct {
		name string
		max  int64
	}{{"manifest.json", maxExperimentManifestBytes}, {"summary.csv", guiCSVMaxBytes}} {
		if _, err := guiReadRegular(dir, file.name, file.max); err != nil && !errors.Is(err, os.ErrNotExist) {
			return guiExperimentRecord{}, err
		}
	}
	select {
	case m.activeSlots <- struct{}{}:
	default:
		return guiExperimentRecord{}, errors.New("at most two GUI experiments may run at once")
	}
	record.State, record.Error, record.UpdatedAt = "running", "", time.Now().UTC()
	if err := m.saveRecordLocked(record); err != nil {
		<-m.activeSlots
		return guiExperimentRecord{}, err
	}
	runCtx, runCancel := context.WithTimeout(m.ctx, guiRunTimeout)
	m.active[id] = runCancel
	m.cancelled[id] = false
	m.wg.Add(1)
	go m.run(runCtx, id, inputPath, manifestPath, csvPath, true)
	return record, nil
}

func (m *guiExperimentManager) Cancel(id string) (guiExperimentRecord, error) {
	if !guiExperimentIDPattern.MatchString(id) {
		return guiExperimentRecord{}, jobs.ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[id]
	if !ok {
		return guiExperimentRecord{}, jobs.ErrNotFound
	}
	cancel := m.active[id]
	if cancel == nil || record.State != "running" {
		return record, nil
	}
	m.cancelled[id] = true
	cancel()
	return record, nil
}

func (m *guiExperimentManager) Export(id string) (guiExperimentExport, error) {
	if !guiExperimentIDPattern.MatchString(id) {
		return guiExperimentExport{}, jobs.ErrNotFound
	}
	select {
	case m.exportGate <- struct{}{}:
		defer func() { <-m.exportGate }()
	default:
		return guiExperimentExport{}, jobs.ErrQueueFull
	}
	m.mu.Lock()
	record, ok := m.records[id]
	if !ok {
		m.mu.Unlock()
		return guiExperimentExport{}, jobs.ErrNotFound
	}
	if record.State == "running" {
		m.mu.Unlock()
		return guiExperimentExport{}, errors.New("a running experiment cannot be exported")
	}
	m.mu.Unlock()
	_, manifestPath, _, dir, err := m.paths(id)
	if err != nil {
		return guiExperimentExport{}, err
	}
	manifestBytes, err := guiReadRegular(dir, "manifest.json", maxExperimentManifestBytes)
	if err != nil {
		return guiExperimentExport{}, fmt.Errorf("experiment manifest: %w", err)
	}
	var manifest experimentManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return guiExperimentExport{}, jobs.ErrConflict
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || !manifest.Complete || manifest.Name != "web-"+id || validateExportExperimentManifest(manifest) != nil {
		return guiExperimentExport{}, jobs.ErrConflict
	}
	manifestHash := sha256.Sum256(manifestBytes)
	manifestHashHex := hex.EncodeToString(manifestHash[:])
	bundleRel := filepath.Join(id, "bundles-"+manifestHashHex[:16])
	bundlePath := filepath.Join(m.root, bundleRel)
	if _, err := os.Lstat(bundlePath); err == nil {
		return guiReadExportIndex(bundlePath, manifestHashHex)
	} else if !errors.Is(err, os.ErrNotExist) {
		return guiExperimentExport{}, err
	}
	if err := guiEnsureRealDir(dir); err != nil {
		return guiExperimentExport{}, err
	}
	if err := requireRealExportDirectory(m.root); err != nil {
		return guiExperimentExport{}, err
	}
	var stdout, stderr guiBoundedBuffer
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
	defer cancel()
	code := executeExperimentExport(ctx, []string{"--experiment", manifestPath, "--artifacts", m.artifactsRoot(), "--output", bundlePath}, &stdout, &stderr)
	if code != 0 && code != 2 {
		return guiExperimentExport{}, errors.New(guiSafeMessage(stderr.String(), "evidence export failed"))
	}
	exported, err := guiParseExportOutput(stdout.Bytes(), bundlePath, manifestHashHex)
	if err != nil {
		return guiExperimentExport{}, err
	}
	m.mu.Lock()
	if current, exists := m.records[id]; exists {
		current.Exported, current.Omitted, current.UpdatedAt = exported.Exported, exported.Omitted, time.Now().UTC()
		if err := m.saveRecordLocked(current); err != nil {
			m.mu.Unlock()
			return guiExperimentExport{}, err
		}
		m.records[id] = current
	}
	m.mu.Unlock()
	return exported, nil
}

func (m *guiExperimentManager) readJSONCSV(id, kind string) ([]byte, string, error) {
	if !guiExperimentIDPattern.MatchString(id) {
		return nil, "", jobs.ErrNotFound
	}
	var name, contentType string
	var limit int64
	switch kind {
	case "manifest", "json":
		name, contentType, limit = "manifest.json", "application/json; charset=utf-8", maxExperimentManifestBytes
	case "summary", "csv":
		name, contentType, limit = "summary.csv", "text/csv; charset=utf-8", guiCSVMaxBytes
	default:
		return nil, "", errors.New("only manifest JSON and summary CSV can be downloaded")
	}
	_, _, _, dir, err := m.paths(id)
	if err != nil {
		return nil, "", err
	}
	b, err := guiReadRegular(dir, name, limit)
	if err != nil {
		return nil, "", err
	}
	if name == "manifest.json" && !json.Valid(b) {
		return nil, "", errors.New("experiment manifest is corrupt")
	}
	return b, contentType, nil
}

func (m *guiExperimentManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, cancel := range m.active {
		cancel()
	}
	m.cancel()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	timer := time.NewTimer(32 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (m *guiExperimentManager) run(ctx context.Context, id, inputPath, manifestPath, csvPath string, resume bool) {
	defer m.wg.Done()
	defer func() { <-m.activeSlots }()
	defer func() {
		if recovered := recover(); recovered != nil {
			m.finishRunPanic(id)
		}
		m.mu.Lock()
		delete(m.active, id)
		delete(m.cancelled, id)
		m.mu.Unlock()
	}()
	args := []string{"--url", m.apiURL, "--file", inputPath, "--output", manifestPath, "--csv", csvPath, "--wait", "--timeout", guiRunTimeout.String()}
	if resume {
		args = append(args, "--resume")
	}
	var stdout, stderr guiBoundedBuffer
	code := executeExperiment(ctx, args, &stdout, &stderr)
	if ctx.Err() != nil {
		m.mu.Lock()
		userCanceled := m.cancelled[id]
		m.mu.Unlock()
		if userCanceled {
			m.cancelExperimentJobs(id)
		}
	}
	complete := false
	if data, err := guiReadRegular(filepath.Dir(manifestPath), "manifest.json", maxExperimentManifestBytes); err == nil {
		var manifest experimentManifest
		if json.Unmarshal(data, &manifest) == nil {
			complete = manifest.Complete
		}
	}
	m.mu.Lock()
	record, ok := m.records[id]
	if ok {
		switch {
		case m.cancelled[id]:
			record.State = "canceled"
			record.Error = "experiment canceled; submitted jobs were asked to stop"
		case ctx.Err() != nil && m.ctx.Err() != nil:
			record.State = "interrupted"
			record.Error = "collection interrupted by application shutdown; resume to continue"
		case complete && (code == 0 || code == 2):
			record.State = "completed"
			record.Error = ""
		case ctx.Err() != nil:
			record.State = "needs_attention"
			record.Error = "experiment deadline reached; resume to collect remaining jobs"
		default:
			record.State = "needs_attention"
			record.Error = guiSafeMessage(stderr.String(), "experiment needs attention")
		}
		record.UpdatedAt = time.Now().UTC()
		if err := m.saveRecordLocked(record); err != nil {
			record.State = "needs_attention"
			record.Error = "could not save experiment status; check artifact storage, then reload or resume"
			record.UpdatedAt = time.Now().UTC()
			m.records[id] = record
		}
	}
	m.mu.Unlock()
}

func (m *guiExperimentManager) finishRunPanic(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[id]
	if !ok {
		return
	}
	record.State = "needs_attention"
	record.Error = "experiment coordinator stopped unexpectedly; resume to collect remaining jobs"
	record.UpdatedAt = time.Now().UTC()
	if err := m.saveRecordLocked(record); err != nil {
		record.Error = "experiment coordinator stopped and status could not be saved; check artifact storage"
		m.records[id] = record
	}
}

func (m *guiExperimentManager) cancelExperimentJobs(id string) {
	_, manifestPath, _, dir, err := m.paths(id)
	if err != nil {
		return
	}
	data, err := guiReadRegular(dir, "manifest.json", maxExperimentManifestBytes)
	if err != nil {
		return
	}
	var manifest experimentManifest
	if json.Unmarshal(data, &manifest) != nil {
		return
	}
	api, err := client.New(m.apiURL)
	if err != nil {
		return
	}
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, entry := range manifest.Entries {
		if ctx.Err() != nil {
			return
		}
		if entry.Job == nil || !guiJobActive(entry.Job.Status) {
			continue
		}
		_, _ = api.Cancel(ctx, entry.Job.ID)
	}
	_ = manifestPath
}

func guiNormalizeDraft(draft guiDraft, id string) (guiDraft, experimentInput, error) {
	draft.Title = strings.TrimSpace(draft.Title)
	if draft.Title == "" || len([]rune(draft.Title)) > 120 || strings.ContainsAny(draft.Title, "\x00\r\n") {
		return draft, experimentInput{}, errors.New("title must contain 1..120 characters")
	}
	if len(draft.Tasks) == 0 || len(draft.Tasks) > maxExperimentJobs || draft.Repeats < 1 || draft.Repeats > 100 {
		return draft, experimentInput{}, errors.New("select tasks and set repeats between 1 and 100")
	}
	for i := range draft.Tasks {
		draft.Tasks[i] = strings.TrimSpace(draft.Tasks[i])
	}
	models := make([]string, len(draft.Models))
	for i, model := range draft.Models {
		models[i] = strings.TrimSpace(model)
	}
	draft.Models = models
	variants := make([]experimentVariant, 0, len(draft.Models)+1)
	if len(draft.Models) == 0 {
		variants = append(variants, experimentVariant{Agent: "scripted", Script: "baseline"})
	} else {
		seen := make(map[string]bool, len(draft.Models))
		for _, model := range draft.Models {
			if model == "" || seen[model] {
				return draft, experimentInput{}, errors.New("model names must be non-empty and unique")
			}
			seen[model] = true
			variants = append(variants, experimentVariant{Agent: "llm", Model: model})
		}
		if draft.Baseline {
			variants = append(variants, experimentVariant{Agent: "scripted", Script: "baseline"})
		}
	}
	input := experimentInput{SchemaVersion: 1, Name: "web-" + id, Tasks: append([]string(nil), draft.Tasks...), Variants: variants, Repeats: draft.Repeats}
	if err := validateExperiment(&input); err != nil {
		return draft, experimentInput{}, err
	}
	return draft, input, nil
}

func guiSameDraft(a, b guiDraft) bool {
	aBytes, _ := json.Marshal(a)
	bBytes, _ := json.Marshal(b)
	return bytes.Equal(aBytes, bBytes)
}

func (m *guiExperimentManager) paths(id string) (input, manifest, csv, dir string, err error) {
	if !guiExperimentIDPattern.MatchString(id) {
		err = jobs.ErrNotFound
		return
	}
	dir = filepath.Join(m.root, id)
	if err = requireRealExportDirectory(m.root); err != nil {
		return
	}
	if _, inspectErr := os.Lstat(dir); inspectErr == nil {
		if err = requireRealExportDirectory(dir); err != nil {
			return
		}
	} else if !errors.Is(inspectErr, os.ErrNotExist) {
		err = inspectErr
		return
	}
	input = filepath.Join(dir, "input.json")
	manifest = filepath.Join(dir, "manifest.json")
	csv = filepath.Join(dir, "summary.csv")
	return
}

func (m *guiExperimentManager) artifactsRoot() string { return filepath.Dir(filepath.Dir(m.root)) }

func (m *guiExperimentManager) loadRecords() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshRecordsLocked()
}

func (m *guiExperimentManager) refreshRecordsLocked() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !guiExperimentIDPattern.MatchString(entry.Name()) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return errors.New("GUI experiment store contains a link or non-directory")
		}
		if _, exists := m.records[entry.Name()]; exists {
			continue
		}
		b, err := guiReadRegular(m.root, filepath.ToSlash(filepath.Join(entry.Name(), "record.json")), guiRecordMaxBytes)
		if err != nil {
			return fmt.Errorf("read GUI experiment record: %w", err)
		}
		var record guiExperimentRecord
		if json.Unmarshal(b, &record) != nil || record.ID != entry.Name() || !guiKnownState(record.State) {
			return errors.New("GUI experiment record is invalid")
		}
		if record.State == "running" {
			record.State = "interrupted"
			record.Error = "application restarted; resume to continue collecting jobs"
			record.UpdatedAt = time.Now().UTC()
			if err := m.saveRecordLocked(record); err != nil {
				return err
			}
		}
		m.records[record.ID] = record
	}
	if len(m.records) > guiExperimentMaxCount {
		return errors.New("GUI experiment storage exceeds the 500 experiment limit")
	}
	return nil
}

func (m *guiExperimentManager) saveRecordLocked(record guiExperimentRecord) error {
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > guiRecordMaxBytes {
		return errors.New("GUI experiment record exceeds size limit")
	}
	if err := guiAtomicWrite(m.root, filepath.ToSlash(filepath.Join(record.ID, "record.json")), append(b, '\n'), guiRecordMaxBytes); err != nil {
		return err
	}
	m.records[record.ID] = record
	return nil
}

func guiEnsureRealDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(abs)
	if abs != parent {
		if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
			if err := guiEnsureRealDir(parent); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if st, err := os.Lstat(abs); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("directory path contains a link or non-directory")
		}
		return requireRealExportDirectory(abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := requireRealExportDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(abs, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return requireRealExportDirectory(abs)
}

func guiAtomicWrite(root, rel string, data []byte, max int64) error {
	if int64(len(data)) > max {
		return errors.New("file exceeds size limit")
	}
	if err := requireRealExportDirectory(root); err != nil {
		return err
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return errors.New("invalid relative GUI store path")
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootHandle.Close()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	tmp := clean + ".tmp-" + hex.EncodeToString(random[:])
	f, err := rootHandle.OpenFile(filepath.FromSlash(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = rootHandle.Remove(filepath.FromSlash(tmp))
		return err
	}
	if err := rootHandle.Rename(filepath.FromSlash(tmp), filepath.FromSlash(clean)); err != nil {
		_ = rootHandle.Remove(filepath.FromSlash(tmp))
		return err
	}
	return nil
}

func guiReadRegular(root, rel string, max int64) ([]byte, error) {
	return exportReadRegular(root, filepath.ToSlash(rel), max)
}

func guiJobActive(status string) bool {
	return status == jobs.StatusQueued || status == jobs.StatusRunning
}

func guiKnownState(state string) bool {
	switch state {
	case "running", "completed", "needs_attention", "canceled", "interrupted":
		return true
	default:
		return false
	}
}

func guiSafeMessage(value, fallback string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' || r >= 0x20 {
			b.WriteRune(r)
		}
		if b.Len() >= 900 {
			break
		}
	}
	value = strings.TrimSpace(b.String())
	if value == "" {
		return fallback
	}
	return value
}

func sortGUIRecords(records []guiExperimentRecord) {
	for i := 1; i < len(records); i++ {
		for j := i; j > 0 && records[j].UpdatedAt.After(records[j-1].UpdatedAt); j-- {
			records[j], records[j-1] = records[j-1], records[j]
		}
	}
}

type guiBoundedBuffer struct{ bytes.Buffer }

func (b *guiBoundedBuffer) Write(p []byte) (int, error) {
	remaining := guiOutputMaxBytes - b.Len()
	if remaining > 0 {
		keep := len(p)
		if keep > remaining {
			keep = int(remaining)
		}
		_, _ = b.Buffer.Write(p[:keep])
	}
	return len(p), nil
}

func guiParseExportOutput(data []byte, path, manifestSHA256 string) (guiExperimentExport, error) {
	var result struct {
		Output     string `json:"output"`
		RunBundles int    `json:"run_bundles"`
		Omitted    int    `json:"omitted_count"`
	}
	if len(data) == 0 || json.Unmarshal(data, &result) != nil || result.Output != path {
		return guiExperimentExport{}, errors.New("evidence export returned an invalid result")
	}
	return guiReadExportIndex(path, manifestSHA256)
}

func guiReadExportIndex(path, manifestSHA256 string) (guiExperimentExport, error) {
	if err := requireRealExportDirectory(path); err != nil {
		return guiExperimentExport{}, err
	}
	b, err := guiReadRegular(path, "index.json", 32<<20)
	if err != nil {
		return guiExperimentExport{}, err
	}
	var index struct {
		ManifestSHA256 string `json:"manifest_sha256"`
		Records        []struct {
			Status string `json:"status"`
		} `json:"records"`
	}
	if json.Unmarshal(b, &index) != nil || index.ManifestSHA256 != manifestSHA256 {
		return guiExperimentExport{}, errors.New("evidence export index is invalid")
	}
	result := guiExperimentExport{Path: path}
	for _, item := range index.Records {
		if item.Status == "exported" || item.Status == "reused" {
			result.Exported++
		} else {
			result.Omitted++
		}
	}
	return result, nil
}

var _ io.Writer = (*guiBoundedBuffer)(nil)
