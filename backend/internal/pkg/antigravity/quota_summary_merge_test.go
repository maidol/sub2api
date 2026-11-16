//go:build unit

package antigravity

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type quotaSummaryTransport func(*http.Request) (*http.Response, error)

func (f quotaSummaryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQuotaSummaryMergeFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		transportErr error
		wantCalls    int
		wantError    bool
	}{
		{name: "network", transportErr: errors.New("connection refused"), wantCalls: 2},
		{name: "server", status: 503, wantCalls: 2},
		{name: "forbidden", status: 403, wantCalls: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &Client{httpClient: &http.Client{Transport: quotaSummaryTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, BaseURLs[calls-1]+"/v1internal:retrieveUserQuotaSummary", r.URL.String())
				status := http.StatusOK
				if calls == 1 {
					if tc.transportErr != nil {
						return nil, tc.transportErr
					}
					status = tc.status
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"groups":[]}`)), Header: make(http.Header)}, nil
			})}}
			summary, err := client.RetrieveUserQuotaSummary(t.Context(), "test-token", "test-project")
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, summary)
			}
			require.Equal(t, tc.wantCalls, calls)
		})
	}
}

func TestQuotaSummaryMergeBodyLimit(t *testing.T) {
	client := &Client{httpClient: &http.Client{Transport: quotaSummaryTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", quotaSummaryBodyLimit+1))), Header: make(http.Header)}, nil
	})}}
	_, err := client.RetrieveUserQuotaSummaryFromURL(t.Context(), "https://quota.invalid", "test-token", "test-project")
	require.ErrorContains(t, err, "exceeds")
}
