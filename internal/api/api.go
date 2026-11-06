// Package api exposes the local job HTTP API.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

const maxConcurrentRequests = 64
const maxRequestBytes = 1 << 20
const requestTimeout = 15 * time.Second
const requestIDHeader = "X-Request-ID"

type Backend interface {
	Submit(context.Context, jobs.Request, string) (jobs.Job, bool, error)
	Get(context.Context, string) (jobs.Job, error)
	List(context.Context, int, string) ([]jobs.Job, error)
	Cancel(context.Context, string) (jobs.Job, error)
	Tasks() []jobs.TaskInfo
	Events(context.Context, string, int, int) (jobs.EventPage, error)
	Health(context.Context) error
	Ready(context.Context) error
	Summary(context.Context) (jobs.Summary, error)
}

type Control interface {
	Handle(http.ResponseWriter, *http.Request) bool
}

type Handler struct {
	control Control
	backend Backend
	ui      http.Handler
	gate    chan struct{}
}

// New builds the local-only HTTP API handler.
func New(backend Backend) http.Handler { return NewWithUI(backend, nil) }

// NewWithUI serves an optional embedded controller UI behind the same local
// host/origin/request capacity boundary as the existing job API.
func NewWithUI(backend Backend, ui http.Handler) http.Handler {
	return NewWithControl(backend, ui, nil)
}

// NewWithControl installs authorization after the local HTTP security boundary.
func NewWithControl(backend Backend, ui http.Handler, control Control) http.Handler {
	h := &Handler{backend: backend, ui: ui, control: control, gate: make(chan struct{}, maxConcurrentRequests)}
	return http.HandlerFunc(h.serve)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	requestID, err := newRequestID()
	if err != nil {
		http.Error(w, "request initialization failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set(requestIDHeader, requestID)
	w = &responseWriter{ResponseWriter: w, requestID: requestID}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	default:
		writeError(w, http.StatusServiceUnavailable, "overloaded", "API request capacity is temporarily exhausted")
		return
	}
	if h.backend == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is unavailable")
		return
	}
	if !loopbackRequest(r) {
		writeError(w, http.StatusForbidden, "loopback_only", "API accepts loopback requests only")
		return
	}
	if !sameOriginOrAbsent(r) {
		writeError(w, http.StatusForbidden, "cross_origin", "cross-origin requests are not accepted")
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.control != nil && h.control.Handle(w, r) {
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	switch {
	case path == "/healthz" && r.Method == http.MethodGet:
		h.health(w, r, false)
	case path == "/readyz" && r.Method == http.MethodGet:
		h.health(w, r, true)
	case path == "/metrics" && r.Method == http.MethodGet:
		h.metrics(w, r)
	case path == "/v1/tasks" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"tasks": h.backend.Tasks()})
	case path == "/v1/preview" && r.Method == http.MethodPost:
		h.preview(w, r)
	case path == "/v1/runs" && r.Method == http.MethodPost:
		h.submit(w, r)
	case path == "/v1/runs" && r.Method == http.MethodGet:
		h.list(w, r)
	case strings.HasPrefix(path, "/v1/runs/"):
		h.runRoute(w, r, strings.TrimPrefix(path, "/v1/runs/"))
	default:
		if h.ui != nil && (path == "/" || strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/v1/ui/")) {
			h.ui.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r) {
		writeError(w, http.StatusUnsupportedMediaType, "content_type", "Content-Type must be application/json")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !jobs.ValidIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key must contain at most 128 printable ASCII characters without leading or trailing spaces")
		return
	}
	var req jobs.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a valid job request")
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return
	}
	if req.SchemaVersion != 0 && req.SchemaVersion != 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "request fields are invalid")
		return
	}
	if req.SchemaVersion == 0 {
		req.SchemaVersion = 1
	}
	if req.Agent == "" {
		req.Agent = "scripted"
	}
	if req.Script == "" && req.Agent == "scripted" {
		req.Script = "success"
	}
	if len(req.TaskID) > 80 || strings.TrimSpace(req.TaskID) == "" || len(req.Agent) > 16 || len(req.Script) > 80 || len(req.Model) > 256 || (req.Agent != "scripted" && req.Agent != "llm") {
		writeError(w, http.StatusBadRequest, "invalid_request", "request fields are invalid")
		return
	}
	if (req.Agent == "llm" && req.Script != "") || (req.Agent == "scripted" && req.Model != "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "request fields are invalid")
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") == "/v1/preview" {
		backend, ok := h.backend.(interface {
			Preview(context.Context, jobs.Request) (jobs.Preview, error)
		})
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "preview unavailable")
			return
		}
		frozen, err := backend.Preview(r.Context(), req)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, frozen)
		return
	}
	job, reused, err := h.backend.Submit(r.Context(), req, key)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	status := http.StatusAccepted
	if reused {
		status = http.StatusOK
	}
	writeJSON(w, status, job)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q, err := strictQuery(r.URL.Query(), map[string]bool{"limit": true, "status": true})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "query parameters are invalid")
		return
	}
	limit := 20
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
			return
		}
	}
	status := q.Get("status")
	if status != "" && !knownStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_request", "status is unknown")
		return
	}
	jobsList, err := h.backend.List(r.Context(), limit, status)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobsList})
}

func (h *Handler) runRoute(w http.ResponseWriter, r *http.Request, tail string) {
	parts := strings.Split(tail, "/")
	if len(parts) == 0 || !validJobID(parts[0]) {
		writeError(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		job, err := h.backend.Get(r.Context(), id)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	switch {
	case parts[1] == "cancel" && r.Method == http.MethodPost:
		if r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "cancel endpoint does not accept a body")
			return
		}
		job, err := h.backend.Cancel(r.Context(), id)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
	case parts[1] == "events" && r.Method == http.MethodGet:
		h.events(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request, id string) {
	q, err := strictQuery(r.URL.Query(), map[string]bool{"after_seq": true, "limit": true})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "query parameters are invalid")
		return
	}
	after := 0
	if raw := q.Get("after_seq"); raw != "" {
		after, err = strconv.Atoi(raw)
		if err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "after_seq must be a non-negative integer")
			return
		}
	}
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
			return
		}
	}
	page, err := h.backend.Events(r.Context(), id, after, limit)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request, ready bool) {
	var err error
	state := "healthy"
	if ready {
		state = "ready"
		err = h.backend.Ready(r.Context())
	} else {
		err = h.backend.Health(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, state+"_failed", "service is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": state})
}

func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	summary, err := h.backend.Summary(r.Context())
	if err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP agenteval_jobs Jobs currently in each state.")
	fmt.Fprintln(w, "# TYPE agenteval_jobs gauge")
	for _, status := range allStatuses {
		fmt.Fprintf(w, "agenteval_jobs{status=%q} %d\n", status, summary.ByStatus[status])
	}
	fmt.Fprintln(w, "# HELP agenteval_workers Configured worker slots.")
	fmt.Fprintln(w, "# TYPE agenteval_workers gauge")
	fmt.Fprintf(w, "agenteval_workers{state=%q} %d\n", "limit", summary.WorkerLimit)
	fmt.Fprintf(w, "agenteval_workers{state=%q} %d\n", "active", summary.ActiveWorkers)
	fmt.Fprintln(w, "# HELP agenteval_draining Whether the service is draining jobs.")
	fmt.Fprintln(w, "# TYPE agenteval_draining gauge")
	draining := 0
	if summary.Draining {
		draining = 1
	}
	fmt.Fprintf(w, "agenteval_draining %d\n", draining)
	fmt.Fprintln(w, "# TYPE agenteval_controller_epoch gauge")
	fmt.Fprintf(w, "agenteval_controller_epoch %d\n", summary.ControllerEpoch)
	fmt.Fprintln(w, "# TYPE agenteval_heartbeat_timestamp_seconds gauge")
	fmt.Fprintf(w, "agenteval_heartbeat_timestamp_seconds %d\n", summary.HeartbeatUnix)
	fmt.Fprintln(w, "# TYPE agenteval_reconciled_containers_total counter")
	fmt.Fprintf(w, "agenteval_reconciled_containers_total %d\n", summary.ReconciledContainers)
	fmt.Fprintln(w, "# TYPE agenteval_reconcile_errors_total counter")
	fmt.Fprintf(w, "agenteval_reconcile_errors_total %d\n", summary.ReconcileErrors)
}

var allStatuses = []string{jobs.StatusQueued, jobs.StatusRunning, jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusTimedOut, jobs.StatusCanceled, jobs.StatusInfraError}

func knownStatus(s string) bool {
	for _, candidate := range allStatuses {
		if s == candidate {
			return true
		}
	}
	return false
}

func strictQuery(values url.Values, allowed map[string]bool) (url.Values, error) {
	for key, entries := range values {
		if !allowed[key] || len(entries) != 1 {
			return nil, errors.New("unknown or repeated query parameter")
		}
	}
	return values, nil
}

func validJobID(id string) bool {
	if len(id) != 24 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isJSON(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	target, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		target = r.Host
	}
	target = strings.Trim(target, "[]")
	if target == "localhost" {
		return true
	}
	targetIP := net.ParseIP(target)
	return targetIP != nil && targetIP.IsLoopback()
}

func sameOriginOrAbsent(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", "request is invalid")
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "job not found")
	case errors.Is(err, jobs.ErrConflict), errors.Is(err, jobs.ErrAlreadyRunning):
		writeError(w, http.StatusConflict, "conflict", "request conflicts with current job state")
	case errors.Is(err, jobs.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "queue_full", "job queue is full")
	case errors.Is(err, jobs.ErrUnavailable), errors.Is(err, jobs.ErrStopping), errors.Is(err, jobs.ErrLeadershipLost):
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is unavailable")
	default:
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is unavailable")
	}
}

type apiError struct {
	SchemaVersion int    `json:"schema_version"`
	RequestID     string `json:"request_id"`
	Error         string `json:"error"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	Retryable     bool   `json:"retryable"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	if status == http.StatusServiceUnavailable && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, apiError{Error: code, Code: code, Message: message, Retryable: status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	requestID := ""
	if tagged, ok := w.(*responseWriter); ok {
		requestID = tagged.requestID
	}
	data, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "response could not be encoded")
		return
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		fields = make(map[string]json.RawMessage)
		fields["data"] = data
	}
	version, _ := json.Marshal(1)
	id, _ := json.Marshal(requestID)
	fields["schema_version"] = version
	fields["request_id"] = id
	data, err = json.Marshal(fields)
	if err != nil {
		http.Error(w, "response could not be encoded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

type responseWriter struct {
	http.ResponseWriter
	requestID string
}

func newRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
