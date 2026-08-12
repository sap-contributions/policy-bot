// Copyright 2026 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v82/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestClient(t *testing.T, mux *http.ServeMux) *github.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	baseURL, err := url.Parse(srv.URL + "/")
	require.NoError(t, err)
	client := github.NewClient(nil)
	client.BaseURL = baseURL
	client.UploadURL = baseURL
	return client
}

func TestIsPullRequestTrigger(t *testing.T) {
	assert.True(t, isPullRequestTrigger("pull_request"))
	assert.True(t, isPullRequestTrigger("pull_request_target"))
	assert.False(t, isPullRequestTrigger("push"))
	assert.False(t, isPullRequestTrigger("schedule"))
	assert.False(t, isPullRequestTrigger("workflow_dispatch"))
	assert.False(t, isPullRequestTrigger(""))
}

func TestIsRetryableError(t *testing.T) {
	assert.True(t, isRetryableError(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusInternalServerError}}))
	assert.True(t, isRetryableError(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}}))
	assert.True(t, isRetryableError(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusGatewayTimeout}}))
	assert.True(t, isRetryableError(&github.RateLimitError{Response: &http.Response{StatusCode: http.StatusForbidden}}))
	assert.False(t, isRetryableError(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnauthorized}}))
	assert.False(t, isRetryableError(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}}))
	assert.False(t, isRetryableError(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}))
}

func TestLookupPRsByHeadSHA(t *testing.T) {
	const targetSHA = "abc123"

	t.Run("returns PRs from API", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/org/repo/commits/"+targetSHA+"/pulls", func(w http.ResponseWriter, r *http.Request) {
			prs := []*github.PullRequest{
				{Number: github.Ptr(1)},
				{Number: github.Ptr(2)},
			}
			w.Header().Set("Content-Type", "application/json")
			data, err := json.Marshal(prs)
			require.NoError(t, err)
			_, _ = w.Write(data)
		})
		client := makeTestClient(t, mux)

		prs, err := lookupPRsByHeadSHA(context.Background(), client, "org", "repo", targetSHA)
		require.NoError(t, err)
		require.Len(t, prs, 2)
	})

	t.Run("returns empty when API returns none", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/org/repo/commits/"+targetSHA+"/pulls", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			data, err := json.Marshal([]*github.PullRequest{})
			require.NoError(t, err)
			_, _ = w.Write(data)
		})
		client := makeTestClient(t, mux)

		prs, err := lookupPRsByHeadSHA(context.Background(), client, "org", "repo", targetSHA)
		require.NoError(t, err)
		assert.Empty(t, prs)
	})

	t.Run("retries on transient 5xx error then succeeds", func(t *testing.T) {
		initialLookupDelay = 0
		t.Cleanup(func() { initialLookupDelay = time.Second })

		calls := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/org/repo/commits/"+targetSHA+"/pulls", func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls < 3 {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			prs := []*github.PullRequest{
				{Number: github.Ptr(1)},
			}
			w.Header().Set("Content-Type", "application/json")
			data, err := json.Marshal(prs)
			require.NoError(t, err)
			_, _ = w.Write(data)
		})
		client := makeTestClient(t, mux)

		prs, err := lookupPRsByHeadSHA(context.Background(), client, "org", "repo", targetSHA)
		require.NoError(t, err)
		require.Len(t, prs, 1)
		assert.Equal(t, 3, calls)
	})

	t.Run("does not retry on non-retryable 4xx error", func(t *testing.T) {
		initialLookupDelay = 0
		t.Cleanup(func() { initialLookupDelay = time.Second })

		calls := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/org/repo/commits/"+targetSHA+"/pulls", func(w http.ResponseWriter, r *http.Request) {
			calls++
			http.Error(w, "forbidden", http.StatusForbidden)
		})
		client := makeTestClient(t, mux)

		_, err := lookupPRsByHeadSHA(context.Background(), client, "org", "repo", targetSHA)
		require.Error(t, err)
		assert.Equal(t, 1, calls)
	})

	t.Run("returns error after max attempts on persistent 5xx", func(t *testing.T) {
		initialLookupDelay = 0
		t.Cleanup(func() { initialLookupDelay = time.Second })

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/org/repo/commits/"+targetSHA+"/pulls", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server error", http.StatusInternalServerError)
		})
		client := makeTestClient(t, mux)

		_, err := lookupPRsByHeadSHA(context.Background(), client, "org", "repo", targetSHA)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("%d attempts", maxLookupAttempts))
	})
}
