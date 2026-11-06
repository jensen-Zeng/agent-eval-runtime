package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

type authStore struct {
	jobs.ManagementStore
	user jobs.User
	err  error
}

func (s authStore) Authenticate(context.Context, string) (jobs.User, error) { return s.user, s.err }

func TestPasswords(t *testing.T) {
	encoded, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "correct horse battery staple" || !checkPassword("correct horse battery staple", encoded) || checkPassword("incorrect password", encoded) {
		t.Fatal("password verification failed")
	}
	other, err := hashPassword("correct horse battery staple")
	if err != nil || other == encoded {
		t.Fatal("password salt reused")
	}
	if _, err := hashPassword("short"); !errors.Is(err, jobs.ErrInvalidRequest) {
		t.Fatal("short password accepted")
	}
	for _, value := range []string{"", "pbkdf2-sha256$1$00$00", strings.Repeat("x", 1024)} {
		if checkPassword("pass", value) {
			t.Fatal("malformed hash accepted")
		}
	}
}
func TestManagementLegacyAndCSRFBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, csrf string
		admin                    bool
		want                     int
	}{
		{"no csrf", "POST", "/v1/ui/settings", "", true, 403},
		{"legacy submission", "POST", "/v1/runs", "1", true, 409},
		{"legacy experiment", "POST", "/v1/ui/experiments", "1", true, 409},
		{"legacy read cannot bypass projects", "GET", "/v1/runs/0123456789abcdef01234567", "", true, 409},
		{"member cannot read globals", "GET", "/v1/ui/state", "", false, 403},
		{"member cannot change provider", "POST", "/v1/ui/settings", "1", false, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(Options{Store: authStore{user: jobs.User{ID: "0123456789abcdef01234567", Admin: tc.admin}}})
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
			r.Header.Set("X-AgentEval-CSRF", tc.csrf)
			w := httptest.NewRecorder()
			if !h.Handle(w, r) || w.Code != tc.want {
				t.Fatalf("handled code=%d want=%d", w.Code, tc.want)
			}
		})
	}
}
func TestManagementPreflightIsAtomicAndFrozen(t *testing.T) {
	s := jobs.TaskSnapshot{TaskRevision: 1, ManifestSHA256: "one"}
	calls := 0
	h := New(Options{Preview: func(_ context.Context, r jobs.Request) (jobs.Preview, error) {
		calls++
		if r.TaskID == "bad" {
			return jobs.Preview{}, jobs.ErrInvalidRequest
		}
		return jobs.Preview{Request: r, TaskSnapshot: s}, nil
	}})
	ds := jobs.DatasetVersion{Tasks: []jobs.DatasetTask{{TaskID: "good", Snapshot: s}, {TaskID: "bad", Snapshot: s}}}
	b := ExperimentRequest{Name: "matrix", Variants: []Variant{{Agent: "scripted", Script: "baseline"}}, Repeats: 3}
	if _, err := h.prepare(context.Background(), b, ds); !errors.Is(err, jobs.ErrInvalidRequest) {
		t.Fatalf("bad task accepted: %v", err)
	}
	if calls != 2 {
		t.Fatalf("preflight calls %d", calls)
	}
	b.Tasks = []string{"good"}
	items, err := h.prepare(context.Background(), b, ds)
	if err != nil || len(items) != 3 {
		t.Fatalf("repeats: %v %d", err, len(items))
	}
	ds.Tasks[0].Snapshot.ManifestSHA256 = "changed"
	if _, err := h.prepare(context.Background(), b, ds); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("dataset drift accepted: %v", err)
	}
}
func TestManagementNoSessionInBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	r.Header.Set("Authorization", "invalid")
	if sessionToken(r) != "" {
		t.Fatal("invalid Authorization silently fell back to cookie identity")
	}
}
