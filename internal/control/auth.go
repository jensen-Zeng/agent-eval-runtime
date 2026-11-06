package control

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

const sessionCookie = "agenteval_session"
const passwordIterations = 600000

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{2,63}$`)

func hashPassword(password string) (string, error) {
	if !utf8.ValidString(password) || len([]rune(password)) < 12 || len(password) > 256 {
		return "", jobs.ErrInvalidRequest
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", jobs.ErrUnavailable
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", jobs.ErrUnavailable
	}
	return "pbkdf2-sha256$600000$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key), nil
}
func checkPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != "600000" {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func sessionToken(r *http.Request) string {
	// One authentication mechanism per request prevents confused identities.
	if a := r.Header.Get("Authorization"); a != "" {
		if !strings.HasPrefix(a, "Bearer ") {
			return ""
		}
		return strings.TrimPrefix(a, "Bearer ")
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}
func (h *Handler) currentUser(r *http.Request) (jobs.User, error) {
	token := sessionToken(r)
	if len(token) != 64 {
		return jobs.User{}, jobs.ErrUnauthorized
	}
	if _, err := hex.DecodeString(token); err != nil {
		return jobs.User{}, jobs.ErrUnauthorized
	}
	return h.opts.Store.Authenticate(r.Context(), tokenDigest(token))
}
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, u jobs.User) error {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return jobs.ErrUnavailable
	}
	raw := hex.EncodeToString(token)
	if err := h.opts.Store.CreateSession(r.Context(), u.ID, tokenDigest(raw)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 12 * 60 * 60})
	return nil
}
func (h *Handler) passwordGate(w http.ResponseWriter) bool {
	// Bounded CPU work and a small deployment-wide login budget. This local
	// service does not trust forwarded client IP headers for rate limiting.
	h.loginMu.Lock()
	if time.Since(h.loginWindow) >= time.Minute {
		h.loginWindow = time.Now()
		h.loginCount = 0
	}
	allowed := h.loginCount < 30
	if allowed {
		h.loginCount++
	}
	h.loginMu.Unlock()
	if !allowed {
		fail(w, jobs.ErrQueueFull)
		return false
	}
	select {
	case h.passwords <- struct{}{}:
		return true
	default:
		fail(w, jobs.ErrQueueFull)
		return false
	}
}
func (h *Handler) auth(w http.ResponseWriter, r *http.Request, action string) {
	if action == "state" && r.Method == http.MethodGet {
		initialized, err := h.opts.Store.HasUsers(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		user, err := h.currentUser(r)
		response := map[string]any{"initialized": initialized}
		if err == nil {
			response["user"] = user
		}
		reply(w, 200, response)
		return
	}
	if r.Method != http.MethodPost {
		fail(w, jobs.ErrNotFound)
		return
	}
	if action == "logout" {
		if !emptyBody(w, r) {
			return
		}
		if token := sessionToken(r); token != "" {
			if err := h.opts.Store.DeleteSession(r.Context(), tokenDigest(token)); err != nil {
				fail(w, err)
				return
			}
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1})
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	if action != "setup" && action != "login" {
		fail(w, jobs.ErrNotFound)
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &b); err != nil {
		fail(w, err)
		return
	}
	if !usernamePattern.MatchString(b.Username) || len(b.Password) > 256 {
		fail(w, jobs.ErrInvalidRequest)
		return
	}
	b.Username = strings.ToLower(b.Username)
	if !h.passwordGate(w) {
		return
	}
	defer func() { <-h.passwords }()
	var user jobs.User
	if action == "setup" {
		encoded, err := hashPassword(b.Password)
		if err != nil {
			fail(w, err)
			return
		}
		user, err = h.opts.Store.Bootstrap(r.Context(), b.Username, encoded)
		if err != nil {
			fail(w, err)
			return
		}
	} else {
		cred, err := h.opts.Store.Credentials(r.Context(), b.Username)
		if err != nil {
			// Match the password work for unknown users without disclosing existence.
			_ = checkPassword(b.Password, "pbkdf2-sha256$600000$00000000000000000000000000000000$0000000000000000000000000000000000000000000000000000000000000000")
			fail(w, jobs.ErrUnauthorized)
			return
		}
		if !checkPassword(b.Password, cred.PasswordHash) {
			fail(w, jobs.ErrUnauthorized)
			return
		}
		user = cred.User
	}
	if err := h.startSession(w, r, user); err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]any{"user": user})
}
func (h *Handler) users(w http.ResponseWriter, r *http.Request, u jobs.User) {
	if !u.Admin {
		fail(w, jobs.ErrForbidden)
		return
	}
	if r.Method == http.MethodGet {
		v, err := h.opts.Store.ListUsers(r.Context(), u.ID)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"users": v})
		return
	}
	if r.Method != http.MethodPost {
		fail(w, jobs.ErrNotFound)
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &b); err != nil {
		fail(w, err)
		return
	}
	if !usernamePattern.MatchString(b.Username) {
		fail(w, jobs.ErrInvalidRequest)
		return
	}
	if !h.passwordGate(w) {
		return
	}
	defer func() { <-h.passwords }()
	encoded, err := hashPassword(b.Password)
	if err != nil {
		fail(w, err)
		return
	}
	user, err := h.opts.Store.CreateUser(r.Context(), u.ID, strings.ToLower(b.Username), encoded)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 201, map[string]any{"user": user})
}
