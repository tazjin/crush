package agent

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/charmbracelet/crush/internal/log"
)

// retry429MaxRetries is how many times a 429 response is retried before the
// response is returned to the provider SDK, whose own retry middleware (and
// the caller) take over from there.
const retry429MaxRetries = 3

// retry429InitialDelay is the backoff used when the response carries no
// usable Retry-After header. The delay doubles on every subsequent retry.
const retry429InitialDelay = 5 * time.Second

// retry429MaxDelay caps the waited-out server hint: a provider asking to
// wait longer than this is not worth blocking a session on, so the 429 is
// surfaced instead.
const retry429MaxDelay = 60 * time.Second

// retry429Transport is an http.RoundTripper that retries HTTP 429 responses
// with backoff before they reach the provider SDK.
//
// This deliberately operates below the SDK error handlers: some inference
// gateways answer 429 with a body that does not deserialize into the error
// shape the provider SDK expects (e.g. "json: cannot unmarshal string into
// Go value of type apierror.Error"). When that happens the SDK drops the
// typed error with its status code, headers, and body, and surfaces the raw
// unmarshal failure, which the retry middleware cannot classify as
// retryable. At the transport layer the status code and Retry-After headers
// are still available regardless of the body's shape, so rate limits can be
// honored and retried for any provider.
type retry429Transport struct {
	base http.RoundTripper
}

// newRetry429Transport wraps base (http.DefaultTransport when nil) in a
// RoundTripper that retries 429 responses.
func newRetry429Transport(base http.RoundTripper) http.RoundTripper {
	return &retry429Transport{base: base}
}

// RoundTrip implements [http.RoundTripper].
func (t *retry429Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	attempt := 0
	resp, err := base.RoundTrip(req)
	for err == nil && isRetryable429(resp) && attempt < retry429MaxRetries {
		delay := retry429Delay(resp, attempt)
		if delay < 0 {
			// The server asks to wait longer than we are willing to block.
			return resp, nil
		}

		// The request body has already been consumed; a fresh copy is
		// required to replay the request. Without GetBody there is no way
		// to reconstruct one, so give up and surface the response.
		if req.GetBody == nil {
			return resp, nil
		}

		slog.Warn("Rate limited; retrying request",
			"method", req.Method,
			"url", req.URL.Redacted(),
			"retry_delay", delay.String(),
			"attempt", attempt+1,
		)

		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best effort drain
		_ = resp.Body.Close()

		if !sleepOrCancel(req, delay) {
			return nil, req.Context().Err()
		}

		retryReq := req.Clone(req.Context())
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, bodyErr
		}
		retryReq.Body = body

		attempt++
		resp, err = base.RoundTrip(retryReq)
	}
	return resp, err
}

// isRetryable429 reports whether the response is a rate limit failure.
func isRetryable429(resp *http.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusTooManyRequests
}

// retry429Delay returns how long to wait before the next attempt, or -1 when
// the server hint exceeds the maximum wait. The delay follows the Retry-After
// or retry-after-ms header when present and reasonable, and otherwise falls
// back to exponential backoff.
func retry429Delay(resp *http.Response, attempt int) time.Duration {
	delay := time.Duration(-1)

	// retry-after-ms is more precise than retry-after and used by e.g. OpenAI.
	if v := headerLastValue(resp.Header, "retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil {
			delay = time.Duration(ms * float64(time.Millisecond))
		}
	}

	if v := headerLastValue(resp.Header, "retry-after"); v != "" && delay < 0 {
		if s, err := strconv.ParseFloat(v, 64); err == nil {
			delay = time.Duration(s * float64(time.Second))
		} else if t, err := http.ParseTime(v); err == nil {
			delay = time.Until(t)
		}
	}

	// A server hint beyond the cap is not worth blocking a session on:
	// signal the caller to surface the response instead of retrying.
	if delay >= 0 {
		if delay > retry429MaxDelay {
			return -1
		}
		if delay > 0 {
			return delay
		}
		// A zero hint gains nothing: fall through to backoff.
	}

	backoff := retry429InitialDelay << attempt //nolint:gosec // bounded by retry429MaxRetries
	return backoff
}

// headerLastValue returns the last value of a header, mirroring how
// response headers are reduced elsewhere in the codebase.
func headerLastValue(header http.Header, key string) string {
	v := header.Values(key)
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

// sleepOrCancel waits for delay, returning false when the request context was
// cancelled first.
func sleepOrCancel(req *http.Request, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-req.Context().Done():
		return false
	case <-timer.C:
		return true
	}
}

// newProviderHTTPClient returns an HTTP client for provider SDKs, with the
// 429 retry transport mounted. In debug mode all traffic is logged too.
func newProviderHTTPClient(debug bool) *http.Client {
	var transport http.RoundTripper = http.DefaultTransport
	if debug {
		transport = log.NewHTTPClient().Transport
	}
	return &http.Client{Transport: newRetry429Transport(transport)}
}

// wrapRetry429 mounts the 429 retry transport on an existing HTTP client,
// composing it over the client's current transport chain.
func wrapRetry429(client *http.Client) *http.Client {
	if client == nil {
		return newProviderHTTPClient(false)
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = newRetry429Transport(transport)
	return client
}
