// Package client is a bounded HTTP client for the loopback runtime API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

const maxResponseBytes = 16 << 20

// Client only talks to a loopback HTTP endpoint.
type Client struct {
	base string
	http *http.Client
}

type APIError struct {
	StatusCode int
	RetryAfter time.Duration
	RequestID  string
	Retryable  bool
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	suffix := requestSuffix(e.RequestID)
	if e.Message != "" {
		return fmt.Sprintf("runtime API returned HTTP %d (%s): %s%s", e.StatusCode, e.Code, e.Message, suffix)
	}
	return fmt.Sprintf("runtime API returned HTTP %d%s", e.StatusCode, suffix)
}

// New validates the base URL and rejects anything except a loopback HTTP origin.
func New(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("URL must be a loopback http origin without a path, credentials, query, or fragment")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("URL must include a loopback host")
	}
	host := strings.ToLower(u.Hostname())
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("URL host must be localhost or a loopback IP address")
		}
	}
	if port := u.Port(); port != "" {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 1 || n > 65535 {
			return nil, errors.New("URL port must be between 1 and 65535")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{
		base: strings.TrimSuffix(u.String(), "/"),
		http: &http.Client{
			Timeout:       15 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Submit(ctx context.Context, req jobs.Request, key string) (jobs.Job, bool, error) {
	var job jobs.Job
	if !jobs.ValidIdempotencyKey(key) {
		return job, false, errors.New("invalid idempotency key")
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if key != "" {
		headers.Set("Idempotency-Key", key)
	}
	reused, err := c.call(ctx, http.MethodPost, "/v1/runs", "", req, headers, &job)
	if err == nil {
		err = validateJob(job)
	}
	return job, reused, err
}

func (c *Client) Get(ctx context.Context, id string) (jobs.Job, error) {
	var job jobs.Job
	_, err := c.call(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), "", nil, nil, &job)
	if err == nil {
		err = validateJob(job)
	}
	return job, err
}

func (c *Client) List(ctx context.Context, limit int, status string) ([]jobs.Job, error) {
	query := url.Values{"limit": []string{strconv.Itoa(limit)}}
	if status != "" {
		query.Set("status", status)
	}
	var result struct {
		Jobs []jobs.Job `json:"jobs"`
	}
	_, err := c.call(ctx, http.MethodGet, "/v1/runs", query.Encode(), nil, nil, &result)
	if err == nil {
		for _, job := range result.Jobs {
			if err = validateJob(job); err != nil {
				return nil, err
			}
		}
	}
	return result.Jobs, err
}

func (c *Client) Cancel(ctx context.Context, id string) (jobs.Job, error) {
	var job jobs.Job
	_, err := c.call(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/cancel", "", nil, nil, &job)
	if err == nil {
		err = validateJob(job)
	}
	return job, err
}

func (c *Client) Events(ctx context.Context, id string, after, limit int) (jobs.EventPage, error) {
	query := url.Values{"after_seq": []string{strconv.Itoa(after)}, "limit": []string{strconv.Itoa(limit)}}
	var page jobs.EventPage
	_, err := c.call(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/events", query.Encode(), nil, nil, &page)
	return page, err
}

func (c *Client) Tasks(ctx context.Context) ([]jobs.TaskInfo, error) {
	var result struct {
		Tasks []jobs.TaskInfo `json:"tasks"`
	}
	_, err := c.call(ctx, http.MethodGet, "/v1/tasks", "", nil, nil, &result)
	return result.Tasks, err
}

// call returns true when a successful POST returned HTTP 200 (an idempotent reuse).
func (c *Client) call(ctx context.Context, method, path, query string, body any, headers http.Header, dst any) (bool, error) {
	endpoint := c.base + path
	if query != "" {
		endpoint += "?" + query
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return false, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("runtime API request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return false, fmt.Errorf("read runtime API response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return false, errors.New("runtime API response exceeds the 16 MiB limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable,
		}
		var detail struct {
			Error     string `json:"error"`
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
			Retryable *bool  `json:"retryable"`
		}
		if json.Unmarshal(data, &detail) == nil {
			apiErr.Code, apiErr.Message, apiErr.RequestID = detail.Code, detail.Message, detail.RequestID
			if detail.Retryable != nil {
				apiErr.Retryable = *detail.Retryable
			}
			if apiErr.RequestID == "" {
				apiErr.RequestID = resp.Header.Get("X-Request-ID")
			}
			if apiErr.Code == "" {
				apiErr.Code = detail.Error
			}
		}
		if apiErr.RequestID == "" {
			apiErr.RequestID = resp.Header.Get("X-Request-ID")
		}
		if apiErr.Code == "" {
			apiErr.Code = "http_error"
		}
		return false, apiErr
	}
	if dst == nil {
		return resp.StatusCode == http.StatusOK, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(dst); err != nil {
		return false, fmt.Errorf("decode runtime API response: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return false, errors.New("runtime API response contains trailing data")
	}
	return resp.StatusCode == http.StatusOK, nil
}

func parseRetryAfter(raw string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds < 0 {
		return 0
	}
	if seconds > 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func requestSuffix(id string) string {
	if id == "" {
		return ""
	}
	return " (request_id=" + id + ")"
}

func validateJob(job jobs.Job) error {
	if len(job.ID) != 24 {
		return errors.New("runtime API returned a job with an invalid ID")
	}
	for _, c := range job.ID {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return errors.New("runtime API returned a job with an invalid ID")
		}
	}
	switch job.Status {
	case jobs.StatusQueued, jobs.StatusRunning, jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusTimedOut, jobs.StatusCanceled, jobs.StatusInfraError:
		return nil
	default:
		return errors.New("runtime API returned a job with a missing or unknown status")
	}
}
