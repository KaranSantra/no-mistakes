package codecommit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func pullRequestContentJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	pr := map[string]any{
		"pullRequestId":     "7",
		"pullRequestStatus": "OPEN",
		"pullRequestTargets": []map[string]any{{
			"repositoryName":       testRepo,
			"sourceReference":      "refs/heads/feature",
			"destinationReference": "refs/heads/main",
		}},
	}
	for key, value := range fields {
		if value == nil {
			delete(pr, key)
			continue
		}
		pr[key] = value
	}
	raw, err := json.Marshal(map[string]any{"pullRequest": pr})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestGetPRContentReadsRawTitleAndDescription(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		response string
		wantBody string
	}{
		{
			name:     "raw description",
			response: pullRequestContentJSON(t, map[string]any{"title": "Human title", "description": "# Human\n\n😀 café\nCloses #7\n\n"}),
			wantBody: "# Human\n\n😀 café\nCloses #7\n\n",
		},
		{
			name:     "omitted description is an empty body",
			response: pullRequestContentJSON(t, map[string]any{"title": "Human title"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := newTestHost(map[string]awsTestResponse{
				awsCmd("get-pull-request --pull-request-id 7"): {stdout: tc.response},
			})
			got, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"})
			if err != nil || got != (scm.PRContent{Title: "Human title", Body: tc.wantBody}) {
				t.Fatalf("GetPRContent() = (%+v, %v), want title and exact body %q", got, err, tc.wantBody)
			}
			if len(fake.calls) != 1 {
				t.Fatalf("GetPRContent() made %d calls, want 1", len(fake.calls))
			}
		})
	}
}

func TestGetPRContentRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()

	otherRepo := strings.Replace(pullRequestContentJSON(t, map[string]any{"title": "T", "description": ""}), testRepo, "Example-Other-Client", 1)
	for _, tc := range []struct {
		name     string
		response awsTestResponse
	}{
		{name: "empty output", response: awsTestResponse{stdout: ""}},
		{name: "null", response: awsTestResponse{stdout: "null"}},
		{name: "no pull request", response: awsTestResponse{stdout: "{}"}},
		{name: "null pull request", response: awsTestResponse{stdout: `{"pullRequest":null}`}},
		{name: "mismatched id", response: awsTestResponse{stdout: pullRequestContentJSON(t, map[string]any{"pullRequestId": "8", "title": "T", "description": ""})}},
		{name: "missing title", response: awsTestResponse{stdout: pullRequestContentJSON(t, map[string]any{"description": "body"})}},
		{name: "non-string description", response: awsTestResponse{stdout: pullRequestContentJSON(t, map[string]any{"title": "T", "description": 42})}},
		{name: "null description", response: awsTestResponse{stdout: `{"pullRequest":{"pullRequestId":"7","title":"T","description":null,"pullRequestTargets":[{"repositoryName":"` + testRepo + `"}]}}`}},
		{name: "no targets", response: awsTestResponse{stdout: pullRequestContentJSON(t, map[string]any{"title": "T", "pullRequestTargets": []any{}})}},
		{name: "another repository", response: awsTestResponse{stdout: otherRepo}},
		{name: "failed transport", response: awsTestResponse{stderr: "throttled", code: 254}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHost(map[string]awsTestResponse{
				awsCmd("get-pull-request --pull-request-id 7"): tc.response,
			})
			if got, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
				t.Fatalf("GetPRContent() = %+v, want error", got)
			}
		})
	}
}
