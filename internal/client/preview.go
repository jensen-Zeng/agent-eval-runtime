package client

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"context"
	"net/http"
)

func (c *Client) Preview(ctx context.Context, req jobs.Request) (jobs.Preview, error) {
	var result jobs.Preview
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	_, err := c.call(ctx, http.MethodPost, "/v1/preview", "", req, headers, &result)
	return result, err
}
