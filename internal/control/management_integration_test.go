package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/api"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	postgres "github.com/jensen-Zeng/agent-eval-runtime/internal/store"
)

type noLegacyBackend struct{ api.Backend }
type managementHTTP struct {
	store    *postgres.Postgres
	handler  http.Handler
	snapshot jobs.TaskSnapshot
}

func newManagementHTTP(t *testing.T) *managementHTTP {
	t.Helper()
	dsn := os.Getenv("AGENTEVAL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set AGENTEVAL_TEST_DATABASE_URL for real management HTTP/PostgreSQL tests")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("open test database")
	}
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	schema := "agenteval_http_" + hex.EncodeToString(bytes)
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal("create test schema:", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := postgres.New(context.Background(), u.String(), 12)
	if err != nil {
		t.Fatal("test store:", err)
	}
	t.Cleanup(func() { _ = store.Close(); _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); _ = db.Close() })
	snap := jobs.TaskSnapshot{TaskRevision: 1, ManifestSHA256: fmt.Sprintf("%064x", 1), SourceSHA256: fmt.Sprintf("%064x", 2), EvaluatorSHA256: fmt.Sprintf("%064x", 3), ImageID: "sha256:test", AgentConfigSHA256: fmt.Sprintf("%064x", 4)}
	managed := New(Options{Store: store, Capacity: 500, Catalog: func() []jobs.TaskInfo {
		return []jobs.TaskInfo{{ID: "sample", Revision: 1, Scripts: []string{"baseline"}}}
	}, Snapshot: func(_ context.Context, id string) (jobs.TaskSnapshot, error) {
		if id != "sample" {
			return jobs.TaskSnapshot{}, jobs.ErrInvalidRequest
		}
		s := snap
		s.AgentConfigSHA256 = ""
		return s, nil
	}, Preview: func(_ context.Context, r jobs.Request) (jobs.Preview, error) {
		if r.TaskID != "sample" || r.Agent != "scripted" || r.Script != "baseline" {
			return jobs.Preview{}, jobs.ErrInvalidRequest
		}
		return jobs.Preview{Request: r, TaskSnapshot: snap}, nil
	}})
	return &managementHTTP{store: store, handler: api.NewWithControl(noLegacyBackend{}, nil, managed), snapshot: snap}
}
func (h *managementHTTP) request(t *testing.T, method, path, cookie, key, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-AgentEval-CSRF", "1")
	if cookie != "" {
		r.Header.Set("Cookie", sessionCookie+"="+cookie)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, want, w.Body.String())
	}
	return w
}
func readObject[T any](t *testing.T, w *httptest.ResponseRecorder, key string) T {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(fields[key], &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func (h *managementHTTP) account(t *testing.T, admin jobs.User, name string) (jobs.User, string) {
	t.Helper()
	ctx := context.Background()
	u, err := h.store.CreateUser(ctx, admin.ID, name, "test-only-credential-not-usable-for-login")
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	raw := hex.EncodeToString(token)
	if err := h.store.CreateSession(ctx, u.ID, tokenDigest(raw)); err != nil {
		t.Fatal(err)
	}
	return u, raw
}
func TestManagementHTTPPermissionsAndAtomicSubmission(t *testing.T) {
	h := newManagementHTTP(t)
	h.request(t, "GET", "/v1/manage/projects", "", "", "", 401)
	setup := h.request(t, "POST", "/v1/manage/auth/setup", "", "", `{"username":"operator","password":"test-password-for-management"}`, 200)
	admin := readObject[jobs.User](t, setup, "user")
	cookies := setup.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie policy missing")
	}
	token := cookies[0].Value
	h.request(t, "POST", "/v1/manage/auth/setup", "", "", `{"username":"intruder","password":"test-password-for-management"}`, 409)
	h.request(t, "POST", "/v1/manage/auth/login", "", "", `{"username":"operator","password":"wrong-password"}`, 401)
	_, executor := h.account(t, admin, "executor")
	_, reader := h.account(t, admin, "reader")
	outsiderUser, outsider := h.account(t, admin, "outsider")
	p := readObject[jobs.Project](t, h.request(t, "POST", "/v1/manage/projects", token, "", `{"name":"one","daily_limit":5,"concurrent_limit":1}`, 201), "project")
	prefix := "/v1/manage/projects/" + p.ID
	h.request(t, "PUT", prefix+"/members", token, "", `{"username":"executor","role":"executor"}`, 200)
	h.request(t, "PUT", prefix+"/members", token, "", `{"username":"reader","role":"reader"}`, 200)
	ds := readObject[jobs.DatasetVersion](t, h.request(t, "POST", prefix+"/datasets", token, "", `{"name":"fixtures","version":"v1","tasks":["sample"]}`, 201), "dataset")
	body := fmt.Sprintf(`{"name":"test","dataset_version_id":%q,"variants":[{"agent":"scripted","script":"baseline"}],"repeats":2}`, ds.ID)
	h.request(t, "GET", prefix, outsider, "", "", 403)
	h.request(t, "GET", prefix+"/datasets", outsider, "", "", 403)
	h.request(t, "POST", prefix+"/experiments", reader, "read-submit", body, 403)
	h.request(t, "PUT", prefix+"/members", executor, "", `{"username":"reader","role":"owner"}`, 403)
	h.request(t, "GET", "/v1/manage/catalog", executor, "", "", 403)
	h.request(t, "POST", prefix+"/experiments", executor, "", body, 400)
	ex := readObject[jobs.Experiment](t, h.request(t, "POST", prefix+"/experiments", executor, "matrix", body, 202), "experiment")
	same := readObject[jobs.Experiment](t, h.request(t, "POST", prefix+"/experiments", executor, "matrix", body, 200), "experiment")
	if ex.ID != same.ID || len(ex.Jobs) != 2 || len(same.Jobs) != 2 || ex.Jobs[0].ID != same.Jobs[0].ID {
		t.Fatal("duplicate submission generated another group")
	}
	h.request(t, "POST", prefix+"/experiments", executor, "matrix", strings.Replace(body, `"repeats":2`, `"repeats":3`, 1), 409)
	h.request(t, "POST", prefix+"/experiments", executor, "over-quota", strings.Replace(body, `"repeats":2`, `"repeats":4`, 1), 429)
	invalid := strings.Replace(body, `"script":"baseline"`, `"script":"missing"`, 1)
	h.request(t, "POST", prefix+"/experiments", executor, "invalid-batch", invalid, 400)
	list := readObject[[]jobs.Experiment](t, h.request(t, "GET", prefix+"/experiments", reader, "", "", 200), "experiments")
	if len(list) != 1 {
		t.Fatal("failed submission left experiment rows")
	}
	usage := readObject[jobs.Usage](t, h.request(t, "GET", prefix+"/usage", reader, "", "", 200), "usage")
	if usage.Reserved != 2 || usage.Used != 0 {
		t.Fatalf("usage=%+v", usage)
	}
	h.request(t, "GET", prefix+"/jobs/"+ex.Jobs[0].ID, outsider, "", "", 403)
	h.request(t, "POST", prefix+"/jobs/"+ex.Jobs[0].ID+"/cancel", reader, "", "{}", 403)
	h.request(t, "POST", prefix+"/experiments/"+ex.ID+"/cancel", executor, "", "{}", 200)
	h.request(t, "POST", prefix+"/experiments/"+ex.ID+"/cancel", executor, "", "{}", 200)
	usage = readObject[jobs.Usage](t, h.request(t, "GET", prefix+"/usage", reader, "", "", 200), "usage")
	if usage.Reserved != 0 || usage.Released != 2 {
		t.Fatalf("double release: %+v", usage)
	}
	foreign, err := h.store.CreateProject(context.Background(), outsiderUser.ID, "foreign", 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	h.request(t, "GET", "/v1/manage/projects/"+foreign.ID, token, "", "", 403)
	h.request(t, "GET", "/v1/manage/projects/"+foreign.ID+"/jobs/"+ex.Jobs[0].ID, outsider, "", "", 404)
	h.request(t, "GET", "/v1/runs/"+ex.Jobs[0].ID, token, "", "", 409)
	h.request(t, "GET", "/v1/ui/state", reader, "", "", 403)
	// CSRF and DNS/origin boundaries apply before account and project logic.
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787"+prefix+"/experiments", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Cookie", sessionCookie+"="+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("missing CSRF header accepted")
	}
	r.Header.Set("X-AgentEval-CSRF", "1")
	r.Header.Set("Origin", "https://attacker.invalid")
	w = httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	h.request(t, "POST", "/v1/manage/auth/logout", token, "", "{}", 200)
	h.request(t, "GET", "/v1/manage/projects", token, "", "", 401)
}

func TestManagementHTTPDatasetGrantAndSessionRevocation(t *testing.T) {
	h := newManagementHTTP(t)
	ctx := context.Background()
	admin, err := h.store.Bootstrap(ctx, "admin", "not-a-login-hash")
	if err != nil {
		t.Fatal(err)
	}
	owner, token := h.account(t, admin, "owner")
	p, err := h.store.CreateProject(ctx, owner.ID, "private", 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/v1/manage/projects/" + p.ID
	h.request(t, "POST", prefix+"/datasets", token, "", `{"name":"steal","version":"v1","tasks":["sample"]}`, 403)
	// Explicit revocation is effective on the next request.
	var raw string
	err = h.store.DeleteSession(ctx, tokenDigest(token))
	if err != nil {
		t.Fatal(err)
	}
	raw = token
	h.request(t, "GET", prefix, raw, "", "", 401)
	// Date validation belongs to the request boundary/store, not SQL interpolation.
	if _, err := h.store.ProjectUsage(ctx, owner.ID, p.ID, "2026-13-99"); err == nil {
		t.Fatal("invalid usage date accepted")
	}

}
