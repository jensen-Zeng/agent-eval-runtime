package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/scheduler"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/webui"
)

var guiIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

type guiPreset struct {
	ID string `json:"id"`
	guiDraft
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type guiTask struct {
	jobs.TaskInfo
	Metadata map[string]string `json:"metadata,omitempty"`
	Budget   domain.Budget     `json:"budget"`
	Limits   any               `json:"limits"`
}
type guiDashboard struct {
	provider    *guiProvider
	manager     *guiExperimentManager
	service     *scheduler.Service
	registry    *taskRegistry
	config      config.Config
	presetsRoot string
	presetMu    sync.Mutex
	operation   chan struct{}
	static      http.Handler
}

func newGUIDashboard(ctx context.Context, project string, c config.Config, service *scheduler.Service, registry *taskRegistry, provider *guiProvider, apiURL string) (*guiDashboard, error) {
	root := filepath.Join(c.ArtifactsDir, "ui", "presets")
	if err := guiEnsureDirectory(root); err != nil {
		return nil, err
	}
	manager, err := newGUIExperimentManager(ctx, c.ArtifactsDir, apiURL)
	if err != nil {
		return nil, err
	}
	return &guiDashboard{provider: provider, manager: manager, service: service, registry: registry, config: c, presetsRoot: root, operation: make(chan struct{}, 1), static: webui.Handler()}, nil
}
func guiEnsureDirectory(root string) error {
	if st, err := os.Lstat(root); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("UI storage must use real directories")
		}
		return requireRealExportDirectory(root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(root)
	if parent == root {
		return fmt.Errorf("UI directory parent is unavailable")
	}
	if err := guiEnsureDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return requireRealExportDirectory(root)
}
func taskDecodeGUI(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("one JSON object is required")
	}
	return nil
}
func guiDecodeRequest(w http.ResponseWriter, r *http.Request, target any) error {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return fmt.Errorf("JSON is required")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		return err
	}
	return taskDecodeGUI(data, target)
}
func guiJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func guiError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, jobs.ErrQueueFull) {
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", "1")
	}
	if errors.Is(err, jobs.ErrConflict) || errors.Is(err, jobs.ErrAlreadyRunning) {
		status = http.StatusConflict
	}
	if errors.Is(err, jobs.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	}
	if errors.Is(err, jobs.ErrUnavailable) {
		status = http.StatusServiceUnavailable
	}
	// Fixed API messages prevent request input, credentials and underlying file
	// contents from reaching UI error payloads or logs.
	message := "操作未完成，请检查输入。连接地址须使用 HTTPS 或本机 HTTP；主密码至少 12 个字符。"
	switch status {
	case http.StatusConflict:
		message = "当前存在运行任务或同名请求冲突，完成或取消运行后再修改连接。"
	case http.StatusTooManyRequests:
		message = "服务正在处理其他操作，请稍后重试。"
	case http.StatusNotFound:
		message = "记录或密钥文件不存在。"
	case http.StatusServiceUnavailable:
		message = "运行环境或模型密钥尚未就绪。"
	}
	guiJSON(w, status, map[string]any{"error": message})
}
func (g *guiDashboard) state(ctx context.Context) (map[string]any, error) {
	summary, err := g.service.Summary(ctx)
	if err != nil {
		return nil, err
	}
	pc, vault := g.provider.state()
	tasks := []guiTask{}
	for _, info := range g.registry.infos {
		t := g.registry.loaded[info.ID]
		tasks = append(tasks, guiTask{info, t.Metadata, t.Budget, t.Limits})
	}
	return map[string]any{"version": runner.Version, "tasks": tasks, "summary": summary, "config": map[string]any{"image": g.config.Image, "llm": pc, "server": map[string]any{"workers": g.config.Server.Workers, "queue_capacity": g.config.Server.QueueCapacity}}, "vault": vault}, nil
}
func (g *guiDashboard) validateDraft(d guiDraft) error {
	if strings.TrimSpace(d.Title) == "" || len([]rune(d.Title)) > 120 || strings.ContainsAny(d.Title, "\x00\r\n") {
		return jobs.ErrInvalidRequest
	}
	input := experimentInput{SchemaVersion: 1, Name: "web-preset", Tasks: d.Tasks, Repeats: d.Repeats}
	for _, model := range d.Models {
		input.Variants = append(input.Variants, experimentVariant{Agent: "llm", Model: model})
	}
	if d.Baseline || len(d.Models) == 0 {
		input.Variants = append(input.Variants, experimentVariant{Agent: "scripted", Script: "baseline"})
	}
	if err := validateExperiment(&input); err != nil {
		return jobs.ErrInvalidRequest
	}
	for _, id := range d.Tasks {
		t, ok := g.registry.loaded[id]
		if !ok {
			return jobs.ErrInvalidRequest
		}
		if d.Baseline || len(d.Models) == 0 {
			if _, ok := t.Scripts["baseline"]; !ok {
				return jobs.ErrInvalidRequest
			}
		}
	}
	return nil
}
func (g *guiDashboard) readPreset(id string) (guiPreset, error) {
	var p guiPreset
	if !guiIDPattern.MatchString(id) {
		return p, jobs.ErrNotFound
	}
	data, err := exportReadRegular(g.presetsRoot, id+".json", 128<<10)
	if err != nil {
		return p, err
	}
	if err := taskDecodeGUI(data, &p); err != nil {
		return p, err
	}
	if p.ID != id {
		return p, jobs.ErrConflict
	}
	return p, nil
}
func (g *guiDashboard) listPresets() ([]guiPreset, error) {
	g.presetMu.Lock()
	defer g.presetMu.Unlock()
	entries, err := os.ReadDir(g.presetsRoot)
	if err != nil {
		return nil, err
	}
	if len(entries) > 501 {
		return nil, fmt.Errorf("preset storage exceeds capacity")
	}
	result := []guiPreset{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			p, err := g.readPreset(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return nil, err
			}
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result, nil
}
func (g *guiDashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/ui/") {
		g.static.ServeHTTP(w, r)
		return
	}
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/ui/"), "/")
	if len(r.URL.RawQuery) > 0 {
		guiError(w, jobs.ErrInvalidRequest)
		return
	}
	if r.Method == http.MethodGet {
		switch path {
		case "state":
			state, err := g.state(r.Context())
			if err != nil {
				guiError(w, err)
				return
			}
			guiJSON(w, 200, state)
			return
		case "presets":
			list, err := g.listPresets()
			if err != nil {
				guiError(w, err)
				return
			}
			guiJSON(w, 200, map[string]any{"presets": list})
			return
		case "experiments":
			list, err := g.manager.List()
			if err != nil {
				guiError(w, err)
				return
			}
			guiJSON(w, 200, map[string]any{"experiments": list})
			return
		}
		if strings.HasPrefix(path, "experiments/") {
			g.experimentRoute(w, r, strings.TrimPrefix(path, "experiments/"))
			return
		}
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(path, "presets/") {
		id := strings.TrimPrefix(path, "presets/")
		g.presetMu.Lock()
		defer g.presetMu.Unlock()
		if _, err := g.readPreset(id); err != nil {
			guiError(w, err)
			return
		}
		root, err := os.OpenRoot(g.presetsRoot)
		if err != nil {
			guiError(w, err)
			return
		}
		defer root.Close()
		if err := root.Remove(id + ".json"); err != nil {
			guiError(w, err)
			return
		}
		guiJSON(w, 200, map[string]any{"deleted": true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch path {
	case "presets":
		var d guiDraft
		if guiDecodeRequest(w, r, &d) != nil {
			guiError(w, jobs.ErrInvalidRequest)
			return
		}
		if err := g.validateDraft(d); err != nil {
			guiError(w, err)
			return
		}
		g.presetMu.Lock()
		defer g.presetMu.Unlock()
		entries, err := os.ReadDir(g.presetsRoot)
		if err != nil || len(entries) >= 500 {
			guiError(w, jobs.ErrQueueFull)
			return
		}
		id, err := experimentID()
		if err != nil {
			guiError(w, err)
			return
		}
		id = id[:24]
		p := guiPreset{id, d, time.Now().UTC(), time.Now().UTC()}
		if err := guiWriteJSON(g.presetsRoot, id+".json", p); err != nil {
			guiError(w, err)
			return
		}
		guiJSON(w, 201, map[string]any{"preset": p})
	case "experiments":
		var body struct {
			PresetID  string `json:"preset_id"`
			RequestID string `json:"request_id"`
		}
		if guiDecodeRequest(w, r, &body) != nil {
			guiError(w, jobs.ErrInvalidRequest)
			return
		}
		p, err := g.readPreset(body.PresetID)
		if err != nil {
			guiError(w, err)
			return
		}
		if err := g.validateDraft(p.guiDraft); err != nil {
			guiError(w, err)
			return
		}
		g.provider.gate.RLock()
		defer g.provider.gate.RUnlock()
		_, vault := g.provider.state()
		if len(p.Models) > 0 && !vault.HasKey {
			guiError(w, jobs.ErrUnavailable)
			return
		}
		record, err := g.manager.Start(r.Context(), p.guiDraft, body.RequestID)
		if err != nil {
			guiError(w, err)
			return
		}
		guiJSON(w, 202, map[string]any{"experiment": record})
	case "settings", "vault/save", "vault/unlock", "vault/lock":
		g.settingsRoute(w, r, path)
	default:
		if strings.HasPrefix(path, "experiments/") {
			g.experimentRoute(w, r, strings.TrimPrefix(path, "experiments/"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}
}
func (g *guiDashboard) settingsRoute(w http.ResponseWriter, r *http.Request, path string) {
	select {
	case g.operation <- struct{}{}:
		defer func() { <-g.operation }()
	default:
		guiError(w, jobs.ErrQueueFull)
		return
	}
	g.provider.gate.Lock()
	defer g.provider.gate.Unlock()
	summary, err := g.service.Summary(r.Context())
	if err != nil {
		guiError(w, err)
		return
	}
	// Unlocking the existing vault preserves the frozen connection/key and must
	// remain available for recovered model jobs waiting for credentials.
	_, currentVault := g.provider.state()
	unlockingExisting := path == "vault/unlock" && currentVault.Exists && !currentVault.Unlocked
	if !unlockingExisting && (summary.Queued > 0 || summary.Running > 0 || g.manager.HasRunning()) {
		guiError(w, jobs.ErrConflict)
		return
	}
	switch path {
	case "settings":
		var c guiProviderConfig
		if guiDecodeRequest(w, r, &c) != nil {
			guiError(w, jobs.ErrInvalidRequest)
			return
		}
		err = g.provider.saveSettings(c)
	case "vault/save":
		var body struct {
			Password string `json:"password"`
			APIKey   string `json:"api_key"`
		}
		if guiDecodeRequest(w, r, &body) != nil {
			guiError(w, jobs.ErrInvalidRequest)
			return
		}
		err = g.provider.saveKey(body.Password, body.APIKey)
		body.Password = ""
		body.APIKey = ""
	case "vault/unlock":
		var body struct {
			Password string `json:"password"`
		}
		if guiDecodeRequest(w, r, &body) != nil {
			guiError(w, jobs.ErrInvalidRequest)
			return
		}
		err = g.provider.unlock(body.Password)
		body.Password = ""
	case "vault/lock":
		var body struct{}
		if guiDecodeRequest(w, r, &body) != nil {
			guiError(w, jobs.ErrInvalidRequest)
			return
		}
		g.provider.lock()
	}
	if err != nil {
		guiError(w, err)
		return
	}
	if path == "settings" {
		state, err := g.state(r.Context())
		if err != nil {
			guiError(w, err)
			return
		}
		guiJSON(w, 200, state)
	} else {
		_, v := g.provider.state()
		guiJSON(w, 200, map[string]any{"vault": v})
	}
}
func (g *guiDashboard) experimentRoute(w http.ResponseWriter, r *http.Request, tail string) {
	parts := strings.Split(tail, "/")
	if len(parts) > 2 || !guiIDPattern.MatchString(parts[0]) {
		guiError(w, jobs.ErrNotFound)
		return
	}
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if r.Method == http.MethodGet {
		if action == "" {
			detail, err := g.manager.Detail(id)
			if err != nil {
				guiError(w, err)
				return
			}
			guiJSON(w, 200, map[string]any{"experiment": detail.Record, "manifest": detail.ManifestRaw})
			return
		}
		if action == "csv" || action == "json" {
			data, _, err := g.manager.readJSONCSV(id, action)
			if err != nil {
				guiError(w, err)
				return
			}
			kind := "application/json"
			ext := "json"
			if action == "csv" {
				kind = "text/csv; charset=utf-8"
				ext = "csv"
			}
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=experiment-%s.%s", id, ext))
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(data)
			return
		}
	}
	if r.Method == http.MethodPost {
		var body struct{}
		if guiDecodeRequest(w, r, &body) != nil {
			guiError(w, jobs.ErrInvalidRequest)
			return
		}
		switch action {
		case "resume":
			g.provider.gate.RLock()
			defer g.provider.gate.RUnlock()
			record, err := g.manager.Resume(id)
			if err != nil {
				guiError(w, err)
				return
			}
			guiJSON(w, 202, map[string]any{"experiment": record})
			return
		case "cancel":
			if _, err := g.manager.Cancel(id); err != nil {
				guiError(w, err)
				return
			}
			guiJSON(w, 200, map[string]any{"canceled": true})
			return
		case "export":
			result, err := g.manager.Export(id)
			if err != nil {
				guiError(w, err)
				return
			}
			guiJSON(w, 200, result)
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}
