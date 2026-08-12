// Copyright 2018 Palantir Technologies, Inc.
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
	stderrors "errors"
	"os"
	"time"

	"github.com/google/go-github/v82/github"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/pull"
	pkgerrors "github.com/pkg/errors"
)

// isPullRequestTrigger returns true for workflow event types that are associated
// with pull requests and therefore should have a non-empty pull_requests field
// in the webhook payload.
func isPullRequestTrigger(event string) bool {
	switch event {
	case "pull_request", "pull_request_target":
		return true
	}
	return false
}

// lookupPRsByHeadSHA queries the GitHub API for PRs associated with the given
// commit SHA. Used as a fallback when the workflow_run event payload omits the
// pull_requests field (upstream issue palantir/policy-bot#960).
// Retries with exponential backoff (capped at maxLookupDelay) on transient
// errors, up to maxLookupAttempts.
const (
	maxLookupAttempts = 10
	maxLookupDelay    = 30 * time.Second
)

var initialLookupDelay = time.Second

func lookupPRsByHeadSHA(ctx context.Context, client *github.Client, owner, name, sha string) ([]*github.PullRequest, error) {
	delay := initialLookupDelay
	for attempt := 1; attempt <= maxLookupAttempts; attempt++ {
		prs, _, err := client.PullRequests.ListPullRequestsWithCommit(ctx, owner, name, sha, &github.ListOptions{
			PerPage: 100,
		})
		if err == nil {
			return prs, nil
		}

		if !isRetryableError(err) || attempt == maxLookupAttempts {
			return nil, pkgerrors.Wrapf(err, "failed to list pull requests after %d attempts", maxLookupAttempts)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < maxLookupDelay {
			delay *= 2
			if delay > maxLookupDelay {
				delay = maxLookupDelay
			}
		}
	}

	// unreachable, but satisfies the compiler
	return nil, pkgerrors.Errorf("failed to list pull requests after %d attempts", maxLookupAttempts)
}

// isRetryableError returns true for transient errors that are worth retrying:
// server-side 5xx errors, rate-limit responses, and timeouts. 4xx client
// errors (bad credentials, not found, forbidden) are not retryable.
func isRetryableError(err error) bool {
	if isServerError(err) || os.IsTimeout(err) {
		return true
	}
	var rateLimitErr *github.RateLimitError
	if stderrors.As(err, &rateLimitErr) {
		return true
	}
	var abuseErr *github.AbuseRateLimitError
	if stderrors.As(err, &abuseErr) {
		return true
	}
	return false
}

type WorkflowRun struct {
	Base
}

func (h *WorkflowRun) Handles() []string { return []string{"workflow_run"} }

func (h *WorkflowRun) Handle(ctx context.Context, eventType, deliveryID string, payload []byte) error {
	// https://docs.github.com/en/actions/using-workflows/events-that-trigger-workflows#workflow_run
	// https://docs.github.com/en/webhooks/webhook-events-and-payloads?actionType=completed#workflow_run
	var event github.WorkflowRunEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return pkgerrors.Wrap(err, "failed to parse workflow_run event payload")
	}

	if event.GetAction() != "completed" {
		return nil
	}

	repo := event.GetRepo()
	repoID := repo.GetID()
	ownerName := repo.GetOwner().GetLogin()
	repoName := repo.GetName()
	commitSHA := event.GetWorkflowRun().GetHeadSHA()
	installationID := githubapp.GetInstallationIDFromEvent(&event)

	ctx, logger := githubapp.PrepareRepoContext(ctx, installationID, repo)

	prs := event.GetWorkflowRun().PullRequests
	if len(prs) == 0 && isPullRequestTrigger(event.GetWorkflowRun().GetEvent()) {
		// GitHub sometimes omits pull_requests from the workflow_run payload for
		// PR-triggered runs (upstream issue palantir/policy-bot#960). Fall back
		// to an API lookup so affected PRs still get re-evaluated.
		client, err := h.ClientCreator.NewInstallationClient(installationID)
		if err != nil {
			return pkgerrors.Wrap(err, "failed to create installation client for PR lookup")
		}
		found, err := lookupPRsByHeadSHA(ctx, client, ownerName, repoName, commitSHA)
		if err != nil {
			logger.Warn().Err(err).Msgf("Failed to look up PRs by head SHA '%s', skipping evaluation", commitSHA)
			return nil
		}
		if len(found) == 0 {
			logger.Debug().Msgf("No open PRs found for SHA '%s', skipping evaluation", commitSHA)
			return nil
		}
		logger.Debug().Msgf("workflow_run payload had no pull_requests; found %d via API lookup for SHA '%s'", len(found), commitSHA)
		prs = found
	}

	evaluationFailures := 0
	for _, pr := range prs {
		// The `workflow_run` event includes pull requests that contain the SHA
		// which is being checked. These can be pull requests _from_ our
		// repository _to_ another one, for example if it's been forked and
		// there's a PR to merge changes from our repo into the fork. We don't
		// want to try to evaluate the policy for such PRs as they're nothing to
		// do with us.
		prBaseRepo := pr.GetBase().GetRepo()
		if prBaseRepo.GetID() != repoID {
			logger.Debug().Msgf("Skipping pull request '%d' from different repository '%s'", pr.GetNumber(), prBaseRepo.GetURL())
			continue
		}

		if pr.GetState() != "open" {
			logger.Debug().Msgf("Skipping pull request '%d' with state '%s'", pr.GetNumber(), pr.GetState())
			continue
		}

		if err := h.Evaluate(ctx, installationID, common.TriggerStatus, pull.Locator{
			Owner:  ownerName,
			Repo:   repoName,
			Number: pr.GetNumber(),
			Value:  pr,
		}); err != nil {
			evaluationFailures++
			logger.Error().Err(err).Msgf("Failed to evaluate pull request '%d' for SHA '%s'", pr.GetNumber(), commitSHA)
		}
	}
	if evaluationFailures == 0 {
		return nil
	}

	return pkgerrors.Errorf("failed to evaluate %d pull requests", evaluationFailures)
}
