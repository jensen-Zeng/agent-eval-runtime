package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/scheduler"
)

const vaultRounds = 600000
const vaultAAD = "agenteval-runtime/api-key-vault/v1"

type guiProviderConfig struct {
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	MaxRetries int    `json:"max_retries"`
}
type guiVaultState struct {
	Exists   bool `json:"exists"`
	Unlocked bool `json:"unlocked"`
	HasKey   bool `json:"has_key"`
}
type guiVaultFile struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	Rounds     int    `json:"rounds"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}
type guiProvider struct {
	mu sync.RWMutex
	// The gate serializes provider changes with API submissions, while mu protects
	// credential reads in both preview and worker execution.
	gate        sync.RWMutex
	config      guiProviderConfig
	root        string
	vault       *guiVaultFile
	key         string
	fallback    string
	lastAttempt time.Time
}

func newGUIProvider(project string, c config.Config) (*guiProvider, error) {
	root := filepath.Join(project, "configs", "ui")
	if err := guiEnsureDirectory(root); err != nil {
		return nil, err
	}
	if err := requireRealExportDirectory(root); err != nil {
		return nil, err
	}
	p := &guiProvider{root: root, config: guiProviderConfig{c.LLM.BaseURL, c.LLM.Model, c.LLM.MaxRetries}, fallback: os.Getenv(c.LLM.APIKeyEnv)}
	if b, err := exportReadRegular(root, "provider.json", 32<<10); err == nil {
		if err = taskDecodeGUI(b, &p.config); err != nil {
			return nil, fmt.Errorf("invalid saved provider settings")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := validateGUIProvider(p.config); err != nil {
		return nil, err
	}
	if b, err := exportReadRegular(root, "keyvault.json", 32<<10); err == nil {
		var v guiVaultFile
		if err = taskDecodeGUI(b, &v); err != nil {
			return nil, fmt.Errorf("invalid encrypted key file")
		}
		if _, _, _, err := vaultParts(v); err != nil {
			return nil, err
		}
		p.vault = &v
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return p, nil
}

func validateGUIProvider(c guiProviderConfig) error {
	if len(c.BaseURL) > 2048 || len(c.Model) > 256 || strings.TrimSpace(c.Model) != c.Model || strings.ContainsAny(c.Model, "\x00\r\n") || c.MaxRetries < 0 || c.MaxRetries > 2 {
		return fmt.Errorf("invalid model connection settings")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("endpoint must be a plain HTTPS base URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return fmt.Errorf("HTTP endpoints are restricted to loopback; use HTTPS for remote models")
}

// waitForKey keeps recovered model jobs pending inside their bounded worker
// until the operator unlocks the existing vault. It never stores a password.
func (p *guiProvider) waitForKey(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		p.mu.RLock()
		locked := p.vault != nil && p.key == ""
		p.mu.RUnlock()
		if !locked {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *guiProvider) snapshot() (guiProviderConfig, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	key := p.key
	if p.vault == nil {
		key = p.fallback
	}
	return p.config, key
}
func (p *guiProvider) state() (guiProviderConfig, guiVaultState) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	has := p.key != ""
	if p.vault == nil {
		has = p.fallback != ""
	}
	return p.config, guiVaultState{Exists: p.vault != nil, Unlocked: p.key != "", HasKey: has}
}
func (p *guiProvider) saveSettings(c guiProviderConfig) error {
	if err := validateGUIProvider(c); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := guiWriteJSON(p.root, "provider.json", c); err != nil {
		return err
	}
	p.config = c
	return nil
}
func validVaultPassword(password string) bool {
	return utf8.ValidString(password) && utf8.RuneCountInString(password) >= 12 && len(password) <= 1024
}
func validVaultKey(key string) bool {
	if len(key) < 1 || len(key) > 8192 || strings.TrimSpace(key) != key {
		return false
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func vaultParts(v guiVaultFile) ([]byte, []byte, []byte, error) {
	if v.Version != 1 || v.Algorithm != "AES-256-GCM/PBKDF2-SHA256" || v.Rounds != vaultRounds {
		return nil, nil, nil, fmt.Errorf("unsupported encrypted key format")
	}
	salt, e1 := base64.StdEncoding.DecodeString(v.Salt)
	nonce, e2 := base64.StdEncoding.DecodeString(v.Nonce)
	data, e3 := base64.StdEncoding.DecodeString(v.Ciphertext)
	if e1 != nil || e2 != nil || e3 != nil || len(salt) != 16 || len(nonce) != 12 || len(data) < 17 || len(data) > 16384 {
		return nil, nil, nil, fmt.Errorf("invalid encrypted key file")
	}
	return salt, nonce, data, nil
}
func vaultCipher(password string, salt []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, vaultRounds, 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func openVault(v guiVaultFile, password string) (string, error) {
	salt, nonce, data, err := vaultParts(v)
	if err != nil {
		return "", err
	}
	a, err := vaultCipher(password, salt)
	if err != nil {
		return "", err
	}
	plain, err := a.Open(nil, nonce, data, []byte(vaultAAD))
	if err != nil {
		return "", fmt.Errorf("master password is incorrect or encrypted key file is damaged")
	}
	defer clear(plain)
	if !validVaultKey(string(plain)) {
		return "", fmt.Errorf("invalid decrypted credential")
	}
	return string(plain), nil
}
func (p *guiProvider) saveKey(password, key string) error {
	if !validVaultPassword(password) || !validVaultKey(key) {
		return fmt.Errorf("use a master password of at least 12 characters and a nonempty printable API key")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.lastAttempt) < time.Second {
		return jobs.ErrQueueFull
	}
	p.lastAttempt = time.Now()
	// Updating an existing vault requires the current master password; a failed
	// unlock never overwrites an existing key. Passwords are never written.
	if p.vault != nil {
		if _, err := openVault(*p.vault, password); err != nil {
			return err
		}
	}
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	a, err := vaultCipher(password, salt)
	if err != nil {
		return err
	}
	sealed := a.Seal(nil, nonce, []byte(key), []byte(vaultAAD))
	v := guiVaultFile{1, "AES-256-GCM/PBKDF2-SHA256", vaultRounds, base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(sealed)}
	if err := guiWriteJSON(p.root, "keyvault.json", v); err != nil {
		return err
	}
	p.vault = &v
	p.key = key
	return nil
}
func (p *guiProvider) unlock(password string) error {
	if !validVaultPassword(password) {
		return fmt.Errorf("invalid master password")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vault == nil {
		return jobs.ErrNotFound
	}
	if time.Since(p.lastAttempt) < time.Second {
		return jobs.ErrQueueFull
	}
	p.lastAttempt = time.Now()
	key, err := openVault(*p.vault, password)
	if err != nil {
		return err
	}
	p.key = key
	return nil
}
func (p *guiProvider) lock() { p.mu.Lock(); defer p.mu.Unlock(); p.key = "" }

type guiGuardBackend struct {
	*scheduler.Service
	provider *guiProvider
}

func (b guiGuardBackend) Submit(ctx context.Context, r jobs.Request, key string) (jobs.Job, bool, error) {
	b.provider.gate.RLock()
	defer b.provider.gate.RUnlock()
	return b.Service.Submit(ctx, r, key)
}
func (b guiGuardBackend) Preview(ctx context.Context, r jobs.Request) (jobs.Preview, error) {
	b.provider.gate.RLock()
	defer b.provider.gate.RUnlock()
	return b.Service.Preview(ctx, r)
}

// guiWriteJSON only publishes a fixed relative filename within an already
// validated operator directory. A same-directory rename keeps readers atomic.
func guiWriteJSON(root, name string, value any) error {
	if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid settings filename")
	}
	if err := requireRealExportDirectory(root); err != nil {
		return err
	}
	target := filepath.Join(root, name)
	if st, err := os.Lstat(target); err == nil {
		if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid settings target")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > 2<<20 {
		return fmt.Errorf("settings exceed size limit")
	}
	f, err := os.CreateTemp(root, ".ui-write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if st, err := os.Lstat(target); err == nil && (st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular()) {
		return fmt.Errorf("settings target changed")
	}
	return os.Rename(f.Name(), target)
}
