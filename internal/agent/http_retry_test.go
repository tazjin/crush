package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// countingTransport serves a fixed sequence of status codes and counts
// requests.
type countingTransport struct {
	statuses []int
	count    int
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status := t.statuses[min(t.count, len(t.statuses)-1)]
	t.count++
	// Echo the request headers so tests can stage Retry-After hints.
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     req.Header.Clone(),
		Body:       io.NopCloser(bytes.NewBufferString("rate limited")),
	}, nil
}

func TestRetry429RetriesAndSucceeds(t *testing.T) {
	t.Parallel()

	transport := &countingTransport{statuses: []int{http.StatusTooManyRequests, http.StatusOK}}
	client := &http.Client{Transport: newRetry429Transport(transport)}

	// Retry-After: 0 keeps the test fast.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com", bytes.NewBufferString("body"))
	require.NoError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("body")), nil
	}
	req.Header.Set("retry-after-ms", "1")

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, transport.count)
}

func TestRetry429GivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()

	transport := &countingTransport{statuses: []int{http.StatusTooManyRequests}}
	client := &http.Client{Transport: newRetry429Transport(transport)}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com", bytes.NewBufferString("body"))
	require.NoError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("body")), nil
	}
	req.Header.Set("retry-after-ms", "1")

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, retry429MaxRetries+1, transport.count)
}

func TestRetry429DoesNotRetryWhenBodyNotReplayable(t *testing.T) {
	t.Parallel()

	transport := &countingTransport{statuses: []int{http.StatusTooManyRequests}}
	client := &http.Client{Transport: newRetry429Transport(transport)}

	// No GetBody: the request body cannot be replayed.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com", http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, 1, transport.count)
}

func TestRetry429DoesNotRetryOtherStatuses(t *testing.T) {
	t.Parallel()

	transport := &countingTransport{statuses: []int{http.StatusInternalServerError}}
	client := &http.Client{Transport: newRetry429Transport(transport)}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, 1, transport.count)
}

func TestRetry429HonorsRetryAfterHeader(t *testing.T) {
	t.Parallel()

	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		if served <= 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`"not the expected error shape"`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newProviderHTTPClient(false)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewBufferString("body"))
	require.NoError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("body")), nil
	}

	start := time.Now()
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, served)
	require.GreaterOrEqual(t, time.Since(start), time.Second)
}

func TestRetry429SurfacesResponseWhenServerWaitsTooLong(t *testing.T) {
	t.Parallel()

	transport := &countingTransport{statuses: []int{http.StatusTooManyRequests}}
	client := &http.Client{Transport: newRetry429Transport(transport)}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com", bytes.NewBufferString("body"))
	require.NoError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("body")), nil
	}
	// Way beyond the maximum: the transport must not block for long.
	req.Header.Set("retry-after-ms", strconv.Itoa(int(2*retry429MaxDelay/time.Millisecond)))

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, 1, transport.count)
}

func TestRetry429RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	transport := &countingTransport{statuses: []int{http.StatusTooManyRequests}}
	client := &http.Client{Transport: newRetry429Transport(transport)}

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.com", bytes.NewBufferString("body"))
	require.NoError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("body")), nil
	}
	req.Header.Set("retry-after-ms", "30000")

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = client.Do(req)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, transport.count)
}
