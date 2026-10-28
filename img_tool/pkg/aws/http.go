package aws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const userAgent = "rules_img"

// ResponseError is returned for any non-2xx HTTP response.
type ResponseError struct {
	StatusCode int
	Body       string
}

func (e *ResponseError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("http status %d", e.StatusCode)
	}
	return fmt.Sprintf("http status %d: %s", e.StatusCode, body)
}

// Do sends the request built by build, retrying transient failures (transport
// errors, 429 and 5xx) up to three times. build is called once per attempt so
// that requests can be re-signed. On success the caller owns resp.Body.
func Do(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(100<<attempt) * time.Millisecond):
			}
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", userAgent)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		lastErr = &ResponseError{StatusCode: resp.StatusCode, Body: string(body)}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// IsStatus reports whether err is a [ResponseError] with the given status.
func IsStatus(err error, status int) bool {
	var re *ResponseError
	return errors.As(err, &re) && re.StatusCode == status
}
