// Package control owns authenticated, project-scoped management requests.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

type Options struct {
	Store        jobs.ManagementStore
	Preview      func(context.Context, jobs.Request) (jobs.Preview, error)
	Snapshot     func(context.Context, string) (jobs.TaskSnapshot, error)
	Catalog      func() []jobs.TaskInfo
	Events       func(context.Context, string, int, int) (jobs.EventPage, error)
	NotifyCancel func(string)
	// LockSubmission holds the provider configuration stable until the DB commit.
	LockSubmission func() func()
	Capacity       int
}

type Handler struct {
	opts        Options
	passwords   chan struct{}
	loginMu     sync.Mutex
	loginWindow time.Time
	loginCount  int
}

func New(opts Options) *Handler { return &Handler{opts: opts, passwords: make(chan struct{}, 2)} }

// Handle runs inside the API's loopback, same-origin and request-limit boundary.
// Returning false delegates an explicitly allowed legacy/static route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if (path == "" || strings.HasPrefix(path, "/assets/")) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		return false
	}
	if (path == "/healthz" || path == "/readyz") && r.Method == http.MethodGet {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if unsafeMethod(r.Method) && r.Header.Get("X-AgentEval-CSRF") != "1" {
		fail(w, jobs.ErrForbidden)
		return true
	}
	if strings.HasPrefix(path, "/v1/manage/auth/") {
		h.auth(w, r, strings.TrimPrefix(path, "/v1/manage/auth/"))
		return true
	}
	user, err := h.currentUser(r)
	if err != nil {
		fail(w, err)
		return true
	}
	if strings.HasPrefix(path, "/v1/manage/") {
		h.manage(w, r, user, strings.TrimPrefix(path, "/v1/manage/"))
		return true
	}
	// Existing unscoped execution APIs must never bypass project accounting.
	if path == "/v1/runs" || strings.HasPrefix(path, "/v1/runs/") {
		reply(w, 409, map[string]any{"error": "project_required", "message": "请使用项目内的任务接口。"})
		return true
	}
	if unsafeMethod(r.Method) && !(user.Admin && (path == "/v1/ui/settings" || strings.HasPrefix(path, "/v1/ui/vault/"))) {
		reply(w, 409, map[string]any{"error": "project_required", "message": "请通过项目中的数据集版本提交或取消实验。旧无项目写入口已关闭。"})
		return true
	}
	if !user.Admin {
		fail(w, jobs.ErrForbidden)
		return true
	}
	return false
}
func unsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return jobs.ErrInvalidRequest
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return jobs.ErrInvalidRequest
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return jobs.ErrInvalidRequest
	}
	return nil
}
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) {
	code, status, message := "unavailable", 503, "服务暂不可用，请稍后重试。"
	switch {
	case errors.Is(err, jobs.ErrUnauthorized):
		code, status, message = "unauthorized", 401, "请登录，或检查账号与密码。"
	case errors.Is(err, jobs.ErrForbidden):
		code, status, message = "forbidden", 403, "没有此项目或操作的权限。"
	case errors.Is(err, jobs.ErrNotFound):
		code, status, message = "not_found", 404, "记录不存在。"
	case errors.Is(err, jobs.ErrInvalidRequest):
		code, status, message = "invalid_request", 400, "请求参数不正确。"
	case errors.Is(err, jobs.ErrConflict):
		code, status, message = "conflict", 409, "记录、版本或幂等键冲突；请刷新后核对。"
	case errors.Is(err, jobs.ErrQuotaExceeded):
		code, status, message = "quota_exceeded", 429, "项目当日任务额度不足，请调整任务数或联系项目所有者。"
	case errors.Is(err, jobs.ErrQueueFull):
		code, status, message = "queue_full", 429, "等待队列已满，请稍后重试。"
	}
	if status == 429 || status == 503 {
		w.Header().Set("Retry-After", "5")
	}
	reply(w, status, map[string]any{"error": code, "message": message})
}
func emptyBody(w http.ResponseWriter, r *http.Request) bool {
	var body struct{}
	if err := decode(w, r, &body); err != nil {
		fail(w, err)
		return false
	}
	return true
}
func validID(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (h *Handler) manage(w http.ResponseWriter, r *http.Request, u jobs.User, path string) {
	ctx := r.Context()
	if path == "catalog" && r.Method == http.MethodGet {
		if !u.Admin {
			fail(w, jobs.ErrForbidden)
			return
		}
		reply(w, 200, map[string]any{"tasks": h.opts.Catalog()})
		return
	}
	if path == "users" {
		h.users(w, r, u)
		return
	}
	if path == "projects" {
		switch r.Method {
		case http.MethodGet:
			projects, err := h.opts.Store.ListProjects(ctx, u.ID)
			if err != nil {
				fail(w, err)
				return
			}
			reply(w, 200, map[string]any{"projects": projects})
		case http.MethodPost:
			var b struct {
				Name       string `json:"name"`
				Daily      int    `json:"daily_limit"`
				Concurrent int    `json:"concurrent_limit"`
			}
			if err := decode(w, r, &b); err != nil {
				fail(w, err)
				return
			}
			p, err := h.opts.Store.CreateProject(ctx, u.ID, b.Name, b.Daily, b.Concurrent)
			if err != nil {
				fail(w, err)
				return
			}
			reply(w, 201, map[string]any{"project": p})
		default:
			fail(w, jobs.ErrNotFound)
		}
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] != "projects" || !validID(parts[1]) {
		fail(w, jobs.ErrNotFound)
		return
	}
	p, err := h.opts.Store.GetProject(ctx, u.ID, parts[1])
	if err != nil {
		fail(w, err)
		return
	}
	if len(parts) == 2 {
		if r.Method == http.MethodGet {
			reply(w, 200, map[string]any{"project": p})
			return
		}
		if r.Method == http.MethodPatch {
			var b struct {
				Daily      int `json:"daily_limit"`
				Concurrent int `json:"concurrent_limit"`
			}
			if err := decode(w, r, &b); err != nil {
				fail(w, err)
				return
			}
			p, err = h.opts.Store.UpdateProjectLimits(ctx, u.ID, p.ID, b.Daily, b.Concurrent)
			if err != nil {
				fail(w, err)
				return
			}
			reply(w, 200, map[string]any{"project": p})
			return
		}
		fail(w, jobs.ErrNotFound)
		return
	}
	switch parts[2] {
	case "members":
		h.members(w, r, u, p, parts[3:])
	case "datasets":
		h.datasets(w, r, u, p, parts[3:])
	case "experiments":
		h.experiments(w, r, u, p, parts[3:])
	case "jobs":
		h.projectJobs(w, r, u, p, parts[3:])
	case "usage":
		if len(parts) != 3 || r.Method != http.MethodGet {
			fail(w, jobs.ErrNotFound)
			return
		}
		if !allowedQuery(r, "day") {
			fail(w, jobs.ErrInvalidRequest)
			return
		}
		usage, err := h.opts.Store.ProjectUsage(ctx, u.ID, p.ID, r.URL.Query().Get("day"))
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"usage": usage})
	default:
		fail(w, jobs.ErrNotFound)
	}
}
func allowedQuery(r *http.Request, names ...string) bool {
	for key, values := range r.URL.Query() {
		ok := false
		for _, name := range names {
			if key == name {
				ok = true
			}
		}
		if !ok || len(values) != 1 {
			return false
		}
	}
	return true
}
func (h *Handler) members(w http.ResponseWriter, r *http.Request, u jobs.User, p jobs.Project, tail []string) {
	ctx := r.Context()
	if len(tail) == 0 && r.Method == http.MethodGet {
		v, err := h.opts.Store.ListMembers(ctx, u.ID, p.ID)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"members": v})
		return
	}
	if p.Role != "owner" {
		fail(w, jobs.ErrForbidden)
		return
	}
	if len(tail) == 0 && r.Method == http.MethodPut {
		var b struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		}
		if err := decode(w, r, &b); err != nil {
			fail(w, err)
			return
		}
		if err := h.opts.Store.SetMember(ctx, u.ID, p.ID, b.Username, b.Role); err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(tail) == 1 && validID(tail[0]) && r.Method == http.MethodDelete {
		if err := h.opts.Store.RemoveMember(ctx, u.ID, p.ID, tail[0]); err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	fail(w, jobs.ErrNotFound)
}
func (h *Handler) datasets(w http.ResponseWriter, r *http.Request, u jobs.User, p jobs.Project, tail []string) {
	ctx := r.Context()
	if len(tail) == 1 && validID(tail[0]) && r.Method == http.MethodGet {
		v, err := h.opts.Store.GetDatasetVersion(ctx, u.ID, p.ID, tail[0])
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"dataset": v})
		return
	}
	if len(tail) != 0 {
		fail(w, jobs.ErrNotFound)
		return
	}
	if r.Method == http.MethodGet {
		v, err := h.opts.Store.ListDatasetVersions(ctx, u.ID, p.ID)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"datasets": v})
		return
	}
	if r.Method != http.MethodPost {
		fail(w, jobs.ErrNotFound)
		return
	}
	if p.Role != "owner" {
		fail(w, jobs.ErrForbidden)
		return
	}
	var b struct {
		Name    string   `json:"name"`
		Version string   `json:"version"`
		Tasks   []string `json:"tasks"`
	}
	if err := decode(w, r, &b); err != nil {
		fail(w, err)
		return
	}
	if len(b.Tasks) == 0 || len(b.Tasks) > 200 {
		fail(w, jobs.ErrInvalidRequest)
		return
	}
	// A project owner can version already assigned tasks. Only the deployment
	// administrator can grant previously unassigned catalog materials.
	granted := map[string]bool{}
	if !u.Admin {
		versions, err := h.opts.Store.ListDatasetVersions(ctx, u.ID, p.ID)
		if err != nil {
			fail(w, err)
			return
		}
		for _, v := range versions {
			for _, t := range v.Tasks {
				granted[t.TaskID] = true
			}
		}
	}
	tasks := make([]jobs.DatasetTask, 0, len(b.Tasks))
	seen := map[string]bool{}
	for _, id := range b.Tasks {
		if seen[id] {
			fail(w, jobs.ErrInvalidRequest)
			return
		}
		seen[id] = true
		if !u.Admin && !granted[id] {
			fail(w, jobs.ErrForbidden)
			return
		}
		snapshot, err := h.opts.Snapshot(ctx, id)
		if err != nil {
			fail(w, err)
			return
		}
		snapshot.AgentConfigSHA256 = ""
		tasks = append(tasks, jobs.DatasetTask{TaskID: id, Snapshot: snapshot})
	}
	version, err := h.opts.Store.CreateDatasetVersion(ctx, u.ID, p.ID, b.Name, b.Version, tasks)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 201, map[string]any{"dataset": version})
}

type ExperimentRequest struct {
	Name             string    `json:"name"`
	DatasetVersionID string    `json:"dataset_version_id"`
	Tasks            []string  `json:"tasks,omitempty"`
	Variants         []Variant `json:"variants"`
	Repeats          int       `json:"repeats"`
}
type Variant struct {
	Agent  string `json:"agent"`
	Script string `json:"script,omitempty"`
	Model  string `json:"model,omitempty"`
}

func (h *Handler) experiments(w http.ResponseWriter, r *http.Request, u jobs.User, p jobs.Project, tail []string) {
	ctx := r.Context()
	if len(tail) == 0 {
		if r.Method == http.MethodGet {
			v, err := h.opts.Store.ListExperiments(ctx, u.ID, p.ID)
			if err != nil {
				fail(w, err)
				return
			}
			reply(w, 200, map[string]any{"experiments": v})
			return
		}
		if r.Method != http.MethodPost {
			fail(w, jobs.ErrNotFound)
			return
		}
		if p.Role == "reader" {
			fail(w, jobs.ErrForbidden)
			return
		}
		var b ExperimentRequest
		if err := decode(w, r, &b); err != nil {
			fail(w, err)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" || !jobs.ValidIdempotencyKey(key) {
			fail(w, jobs.ErrInvalidRequest)
			return
		}
		ds, err := h.opts.Store.GetDatasetVersion(ctx, u.ID, p.ID, b.DatasetVersionID)
		if err != nil {
			fail(w, err)
			return
		}
		if h.opts.LockSubmission != nil {
			unlock := h.opts.LockSubmission()
			defer unlock()
		}
		items, err := h.prepare(ctx, b, ds)
		if err != nil {
			fail(w, err)
			return
		}
		ex, reused, err := h.opts.Store.SubmitExperiment(ctx, u.ID, p.ID, ds.ID, b.Name, key, items, h.opts.Capacity)
		if err != nil {
			fail(w, err)
			return
		}
		status := 202
		if reused {
			status = 200
		}
		reply(w, status, map[string]any{"experiment": ex, "reused": reused})
		return
	}
	if !validID(tail[0]) {
		fail(w, jobs.ErrNotFound)
		return
	}
	if len(tail) == 1 && r.Method == http.MethodGet {
		v, err := h.opts.Store.GetExperiment(ctx, u.ID, p.ID, tail[0])
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"experiment": v})
		return
	}
	if len(tail) == 2 && tail[1] == "cancel" && r.Method == http.MethodPost {
		if p.Role == "reader" {
			fail(w, jobs.ErrForbidden)
			return
		}
		if !emptyBody(w, r) {
			return
		}
		v, err := h.opts.Store.CancelExperiment(ctx, u.ID, p.ID, tail[0])
		if err != nil {
			fail(w, err)
			return
		}
		for _, j := range v.Jobs {
			h.notifyCancel(j)
		}
		reply(w, 200, map[string]any{"experiment": v})
		return
	}
	fail(w, jobs.ErrNotFound)
}
func (h *Handler) prepare(ctx context.Context, b ExperimentRequest, ds jobs.DatasetVersion) ([]jobs.PreparedJob, error) {
	if strings.TrimSpace(b.Name) == "" || len([]rune(b.Name)) > 120 || len(b.Variants) == 0 || len(b.Variants) > 20 || b.Repeats < 1 || b.Repeats > 100 {
		return nil, jobs.ErrInvalidRequest
	}
	available := map[string]jobs.TaskSnapshot{}
	for _, t := range ds.Tasks {
		available[t.TaskID] = t.Snapshot
	}
	ids := b.Tasks
	if len(ids) == 0 {
		for _, t := range ds.Tasks {
			ids = append(ids, t.TaskID)
		}
	}
	if len(ids) == 0 || len(ids) > 200 || len(ids)*len(b.Variants)*b.Repeats > 500 {
		return nil, jobs.ErrInvalidRequest
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, jobs.ErrInvalidRequest
		}
		seen[id] = true
		if _, ok := available[id]; !ok {
			return nil, jobs.ErrInvalidRequest
		}
	}
	uniqueVariants := map[Variant]bool{}
	for _, v := range b.Variants {
		if uniqueVariants[v] || (v.Agent != "scripted" && v.Agent != "llm") || (v.Agent == "scripted" && (v.Script == "" || v.Model != "")) || (v.Agent == "llm" && (v.Model == "" || v.Script != "")) {
			return nil, jobs.ErrInvalidRequest
		}
		uniqueVariants[v] = true
	}
	out := make([]jobs.PreparedJob, 0, len(ids)*len(b.Variants)*b.Repeats)
	// Prepare each distinct task/model once; repeated jobs share the same frozen
	// inputs but get separate job identities in the submission transaction.
	for _, id := range ids {
		for _, v := range b.Variants {
			frozen, err := h.opts.Preview(ctx, jobs.Request{SchemaVersion: 1, TaskID: id, Agent: v.Agent, Script: v.Script, Model: v.Model})
			if err != nil {
				return nil, err
			}
			got, want := frozen.TaskSnapshot, available[id]
			got.AgentConfigSHA256 = ""
			want.AgentConfigSHA256 = ""
			if got != want {
				return nil, jobs.ErrConflict
			}
			for n := 0; n < b.Repeats; n++ {
				out = append(out, jobs.PreparedJob{Request: frozen.Request, Snapshot: frozen.TaskSnapshot})
			}
		}
	}
	return out, nil
}
func (h *Handler) notifyCancel(j jobs.Job) {
	if h.opts.NotifyCancel != nil && j.CancelRequested {
		h.opts.NotifyCancel(j.ID)
	}
}
func (h *Handler) projectJobs(w http.ResponseWriter, r *http.Request, u jobs.User, p jobs.Project, tail []string) {
	ctx := r.Context()
	if len(tail) == 0 && r.Method == http.MethodGet {
		v, err := h.opts.Store.ProjectJobs(ctx, u.ID, p.ID)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"jobs": v})
		return
	}
	if len(tail) < 1 || !validID(tail[0]) {
		fail(w, jobs.ErrNotFound)
		return
	}
	if len(tail) == 1 && r.Method == http.MethodGet {
		v, err := h.opts.Store.GetProjectJob(ctx, u.ID, p.ID, tail[0])
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"job": v})
		return
	}
	if len(tail) == 2 && tail[1] == "cancel" && r.Method == http.MethodPost {
		if p.Role == "reader" {
			fail(w, jobs.ErrForbidden)
			return
		}
		if !emptyBody(w, r) {
			return
		}
		v, err := h.opts.Store.CancelProjectJob(ctx, u.ID, p.ID, tail[0])
		if err != nil {
			fail(w, err)
			return
		}
		h.notifyCancel(v)
		reply(w, 200, map[string]any{"job": v})
		return
	}
	if len(tail) == 2 && tail[1] == "events" && r.Method == http.MethodGet {
		if _, err := h.opts.Store.GetProjectJob(ctx, u.ID, p.ID, tail[0]); err != nil {
			fail(w, err)
			return
		}
		if !allowedQuery(r, "after_seq", "limit") {
			fail(w, jobs.ErrInvalidRequest)
			return
		}
		after, limit := 0, 100
		var err error
		if raw := r.URL.Query().Get("after_seq"); raw != "" {
			after, err = strconv.Atoi(raw)
			if err != nil || after < 0 {
				fail(w, jobs.ErrInvalidRequest)
				return
			}
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				fail(w, jobs.ErrInvalidRequest)
				return
			}
		}
		page, err := h.opts.Events(ctx, tail[0], after, limit)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, page)
		return
	}
	fail(w, jobs.ErrNotFound)
}
