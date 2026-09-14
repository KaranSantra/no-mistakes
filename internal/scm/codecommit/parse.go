package codecommit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// pullRequestResponse is the envelope `aws codecommit get-pull-request` and
// `create-pull-request` print.
type pullRequestResponse struct {
	PullRequest *pullRequest `json:"pullRequest"`
}

// pullRequest is the subset of a CodeCommit pull request we consume.
type pullRequest struct {
	PullRequestID     string              `json:"pullRequestId"`
	Title             string              `json:"title"`
	Description       description         `json:"description"`
	PullRequestStatus string              `json:"pullRequestStatus"` // OPEN | CLOSED
	Targets           []pullRequestTarget `json:"pullRequestTargets"`
}

// description is a pull request description as the AWS CLI prints it. The key
// is absent when a pull request has no description, which leaves the zero
// value; a present key must hold a string, so an explicit null is rejected as
// malformed rather than read as an empty body.
type description string

func (d *description) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		return errors.New("description is null")
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*d = description(s)
	return nil
}

// pullRequestTarget is one source/destination pair of a pull request.
// CodeCommit pull requests have exactly one, in the pull request's own
// repository.
type pullRequestTarget struct {
	RepositoryName       string `json:"repositoryName"`
	SourceReference      string `json:"sourceReference"`      // refs/heads/{branch}
	DestinationReference string `json:"destinationReference"` // refs/heads/{branch}
	MergeMetadata        struct {
		IsMerged bool `json:"isMerged"`
	} `json:"mergeMetadata"`
}

// pullRequestList is the output of `aws codecommit list-pull-requests`. The
// AWS CLI follows nextToken itself, so the list is already complete.
type pullRequestList struct {
	PullRequestIDs []string `json:"pullRequestIds"`
}

// repositoryResponse is the output of `aws codecommit get-repository`.
type repositoryResponse struct {
	RepositoryMetadata *struct {
		RepositoryName string `json:"repositoryName"`
		ARN            string `json:"Arn"`
	} `json:"repositoryMetadata"`
}

// Request documents passed through --cli-input-json.
type (
	createPullRequestInput struct {
		Title       string        `json:"title"`
		Description string        `json:"description"`
		Targets     []targetInput `json:"targets"`
	}
	targetInput struct {
		RepositoryName       string `json:"repositoryName"`
		SourceReference      string `json:"sourceReference"`
		DestinationReference string `json:"destinationReference"`
	}
	updateDescriptionInput struct {
		PullRequestID string `json:"pullRequestId"`
		Description   string `json:"description"`
	}
	updateTitleInput struct {
		PullRequestID string `json:"pullRequestId"`
		Title         string `json:"title"`
	}
)

// normalizePRState maps a validated pull request to a PR state. CodeCommit
// closes a pull request when it is merged, so the target's merge metadata is
// what separates MERGED from CLOSED.
func normalizePRState(pr *pullRequest) scm.PRState {
	if len(pr.Targets) > 0 && pr.Targets[0].MergeMetadata.IsMerged {
		return scm.PRStateMerged
	}
	switch strings.ToUpper(strings.TrimSpace(pr.PullRequestStatus)) {
	case "OPEN":
		return scm.PRStateOpen
	case "CLOSED":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(strings.TrimSpace(pr.PullRequestStatus)))
	}
}

// branchName strips the refs/heads/ prefix CodeCommit reports references with.
func branchName(ref string) string {
	return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
}

// arnRegion returns the region field of a repository ARN
// (arn:{partition}:codecommit:{region}:{account}:{repository}).
func arnRegion(arn string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(arn), ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "codecommit" || !regionPattern.MatchString(parts[3]) {
		return "", false
	}
	return parts[3], true
}
