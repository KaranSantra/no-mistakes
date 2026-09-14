package codecommit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	testRegion   = "us-east-1"
	testProfile  = "AWSAdministratorAccess-123456789012"
	testRepo     = "Example-Payments-Client"
	testARN      = "arn:aws:codecommit:us-east-1:123456789012:Example-Payments-Client"
	testPRURL    = "https://us-east-1.console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/"
	testHeadSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testMergeSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// awsCmd is the invocation key for `aws codecommit <args>` carrying the test
// host's profile and region scope.
func awsCmd(args string) string {
	return "aws codecommit " + args + " --output json --no-cli-pager --no-cli-auto-prompt --profile " + testProfile + " --region " + testRegion
}

func pullRequestJSON(id, status, source, destination string, merged bool) string {
	mergeCommit := ""
	if merged {
		mergeCommit = testMergeSHA
	}
	return fmt.Sprintf(`{"pullRequest":{"pullRequestId":%q,"title":"Add CodeCommit support","description":"body","pullRequestStatus":%q,"pullRequestTargets":[{"repositoryName":%q,"sourceReference":"refs/heads/%s","destinationReference":"refs/heads/%s","sourceCommit":%q,"mergeMetadata":{"isMerged":%t,"mergeCommitId":%q}}]}}`,
		id, status, testRepo, source, destination, testHeadSHA, merged, mergeCommit)
}

func repositoryJSON(arn string) string {
	return `{"repositoryMetadata":{"repositoryName":"` + testRepo + `","Arn":"` + arn + `"}}`
}

func newTestHost(responses map[string]awsTestResponse) (*Host, *fakeAWS) {
	fake := &fakeAWS{responses: responses}
	return New(fake.cmdFactory(), func() bool { return true }, testRegion, testProfile, testRepo), fake
}

func TestProviderAndCapabilities(t *testing.T) {
	t.Parallel()

	h, _ := newTestHost(nil)
	if h.Provider() != scm.ProviderCodeCommit {
		t.Fatalf("Provider() = %q, want %q", h.Provider(), scm.ProviderCodeCommit)
	}
	if caps := h.Capabilities(); caps.MergeableState || caps.FailedCheckLogs || !caps.MergedProof {
		t.Fatalf("Capabilities() = %+v, want merged proof without mergeability or failed-check logs", caps)
	}
}

func TestAvailableProbesRepositoryWithRemoteScope(t *testing.T) {
	t.Parallel()

	h, fake := newTestHost(map[string]awsTestResponse{
		awsCmd("get-repository --repository-name " + testRepo): {stdout: repositoryJSON(testARN)},
	})
	if err := h.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("Available() made %d calls, want 1", len(fake.calls))
	}
}

func TestAvailableReportsMissingCLI(t *testing.T) {
	t.Parallel()

	fake := &fakeAWS{}
	h := New(fake.cmdFactory(), func() bool { return false }, testRegion, testProfile, testRepo)
	err := h.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "aws CLI is not installed") {
		t.Fatalf("Available() error = %v, want missing CLI error", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("Available() ran %d commands without a CLI", len(fake.calls))
	}
}

func TestAvailableReportsUnreadableRepository(t *testing.T) {
	t.Parallel()

	h, _ := newTestHost(map[string]awsTestResponse{
		awsCmd("get-repository --repository-name " + testRepo): {stderr: "Error when retrieving token from sso: Token has expired and refresh failed\n", code: 255},
	})
	err := h.Available(context.Background())
	if err == nil {
		t.Fatal("Available() error = nil, want unreadable repository error")
	}
	for _, want := range []string{testRepo, "profile " + testProfile, "region " + testRegion, "aws sso login --profile '" + testProfile + "'"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Available() error = %q, want it to mention %q", err, want)
		}
	}
}

func TestAvailableSafelyQuotesProfileInSSORemedy(t *testing.T) {
	t.Parallel()

	profile := "AWSAdministratorAccess-123456789012'quoted;$HOME"
	command := "aws codecommit get-repository --repository-name " + testRepo + " --output json --no-cli-pager --no-cli-auto-prompt --profile " + profile + " --region " + testRegion
	fake := &fakeAWS{responses: map[string]awsTestResponse{
		command: {stderr: "token expired", code: 255},
	}}
	h := New(fake.cmdFactory(), func() bool { return true }, testRegion, profile, testRepo)
	err := h.Available(context.Background())
	want := `aws sso login --profile 'AWSAdministratorAccess-123456789012'"'"'quoted;$HOME'`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Available() error = %q, want safely quoted remedy %q", err, want)
	}
}

func TestCommandsRefuseUnnamedProfile(t *testing.T) {
	t.Parallel()

	fake := &fakeAWS{}
	h := New(fake.cmdFactory(), func() bool { return true }, "", "", testRepo)
	err := h.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires an explicit AWS profile") {
		t.Fatalf("Available() error = %v, want explicit-profile refusal", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("Available() ran %d commands with no profile", len(fake.calls))
	}
}

func TestFindPRReturnsSingleMatchAmongOpenPullRequests(t *testing.T) {
	t.Parallel()

	h, fake := newTestHost(map[string]awsTestResponse{
		awsCmd("list-pull-requests --repository-name " + testRepo + " --pull-request-status OPEN"): {stdout: `{"pullRequestIds":["3","12","7"]}`},
		awsCmd("get-pull-request --pull-request-id 3"):                                             {stdout: pullRequestJSON("3", "OPEN", "other", "main", false)},
		awsCmd("get-pull-request --pull-request-id 12"):                                            {stdout: pullRequestJSON("12", "OPEN", "feature/codecommit", "develop", false)},
		awsCmd("get-pull-request --pull-request-id 7"):                                             {stdout: pullRequestJSON("7", "OPEN", "feature/codecommit", "main", false)},
	})

	pr, err := h.FindPR(context.Background(), "feature/codecommit", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	want := scm.PR{Number: "7", URL: testPRURL + "7", BaseBranch: "main"}
	if pr == nil || *pr != want {
		t.Fatalf("FindPR() = %+v, want %+v", pr, want)
	}
	if got := fake.keys(); len(got) != 4 || !strings.Contains(got[1], "--pull-request-id 3") || !strings.Contains(got[2], "--pull-request-id 12") || !strings.Contains(got[3], "--pull-request-id 7") {
		t.Fatalf("FindPR() commands = %q, want list followed by every open pull request", got)
	}
}

func TestFindPRRejectsMultipleMatchingOpenPullRequests(t *testing.T) {
	t.Parallel()

	h, fake := newTestHost(map[string]awsTestResponse{
		awsCmd("list-pull-requests --repository-name " + testRepo + " --pull-request-status OPEN"): {stdout: `{"pullRequestIds":["12","7","3"]}`},
		awsCmd("get-pull-request --pull-request-id 12"):                                            {stdout: pullRequestJSON("12", "OPEN", "feature/codecommit", "main", false)},
		awsCmd("get-pull-request --pull-request-id 7"):                                             {stdout: pullRequestJSON("7", "OPEN", "feature/codecommit", "main", false)},
		awsCmd("get-pull-request --pull-request-id 3"):                                             {stdout: pullRequestJSON("3", "OPEN", "other", "main", false)},
	})

	pr, err := h.FindPR(context.Background(), "feature/codecommit", "main")
	if pr != nil || err == nil {
		t.Fatalf("FindPR() = (%+v, %v), want ambiguity error and no pull request", pr, err)
	}
	for _, want := range []string{"12", "7", "close the extra pull requests", "one remains open for the branch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("FindPR() error = %q, want it to mention %q", err, want)
		}
	}
	if got := fake.keys(); len(got) != 4 {
		t.Fatalf("FindPR() commands = %q, want list followed by every open pull request", got)
	}
}

func TestFindPRReturnsNilWhenNoOpenPullRequestMatches(t *testing.T) {
	t.Parallel()

	list := awsCmd("list-pull-requests --repository-name " + testRepo + " --pull-request-status OPEN")
	for _, tc := range []struct {
		name      string
		responses map[string]awsTestResponse
		base      string
	}{
		{
			name:      "no open pull requests",
			responses: map[string]awsTestResponse{list: {stdout: `{"pullRequestIds":[]}`}},
		},
		{
			name: "other source branch",
			responses: map[string]awsTestResponse{
				list: {stdout: `{"pullRequestIds":["5"]}`},
				awsCmd("get-pull-request --pull-request-id 5"): {stdout: pullRequestJSON("5", "OPEN", "other", "main", false)},
			},
		},
		{
			name: "other base branch when a base is given",
			responses: map[string]awsTestResponse{
				list: {stdout: `{"pullRequestIds":["6"]}`},
				awsCmd("get-pull-request --pull-request-id 6"): {stdout: pullRequestJSON("6", "OPEN", "feature/codecommit", "main", false)},
			},
			base: "develop",
		},
		{
			name: "closed since the listing",
			responses: map[string]awsTestResponse{
				list: {stdout: `{"pullRequestIds":["9"]}`},
				awsCmd("get-pull-request --pull-request-id 9"): {stdout: pullRequestJSON("9", "CLOSED", "feature/codecommit", "main", true)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHost(tc.responses)
			pr, err := h.FindPR(context.Background(), "feature/codecommit", tc.base)
			if err != nil || pr != nil {
				t.Fatalf("FindPR() = (%+v, %v), want (nil, nil)", pr, err)
			}
		})
	}
}

func TestFindPRRejectsIndeterminateResponses(t *testing.T) {
	t.Parallel()

	list := awsCmd("list-pull-requests --repository-name " + testRepo + " --pull-request-status OPEN")
	get := awsCmd("get-pull-request --pull-request-id 9")
	listed := awsTestResponse{stdout: `{"pullRequestIds":["9"]}`}
	for _, tc := range []struct {
		name      string
		responses map[string]awsTestResponse
	}{
		{name: "list fails", responses: map[string]awsTestResponse{list: {stderr: "AccessDeniedException", code: 254}}},
		{name: "list empty", responses: map[string]awsTestResponse{list: {stdout: ""}}},
		{name: "list null", responses: map[string]awsTestResponse{list: {stdout: "null"}}},
		{name: "list without ids", responses: map[string]awsTestResponse{list: {stdout: "{}"}}},
		{name: "list invalid id", responses: map[string]awsTestResponse{list: {stdout: `{"pullRequestIds":["abc"]}`}}},
		{name: "read fails", responses: map[string]awsTestResponse{list: listed, get: {stderr: "throttled", code: 254}}},
		{name: "read without pull request", responses: map[string]awsTestResponse{list: listed, get: {stdout: "{}"}}},
		{name: "read mismatched id", responses: map[string]awsTestResponse{list: listed, get: {stdout: pullRequestJSON("8", "OPEN", "feature/codecommit", "main", false)}}},
		{name: "read without targets", responses: map[string]awsTestResponse{list: listed, get: {stdout: `{"pullRequest":{"pullRequestId":"9","pullRequestStatus":"OPEN","pullRequestTargets":[]}}`}}},
		{name: "read from another repository", responses: map[string]awsTestResponse{list: listed, get: {stdout: strings.Replace(pullRequestJSON("9", "OPEN", "feature/codecommit", "main", false), testRepo, "Example-Other-Client", 1)}}},
		{name: "read without status", responses: map[string]awsTestResponse{list: listed, get: {stdout: strings.Replace(pullRequestJSON("9", "OPEN", "feature/codecommit", "main", false), `"pullRequestStatus":"OPEN",`, "", 1)}}},
		{name: "read without source reference", responses: map[string]awsTestResponse{list: listed, get: {stdout: strings.Replace(pullRequestJSON("9", "OPEN", "feature/codecommit", "main", false), `"sourceReference":"refs/heads/feature/codecommit",`, "", 1)}}},
		{name: "read without destination reference", responses: map[string]awsTestResponse{list: listed, get: {stdout: strings.Replace(pullRequestJSON("9", "OPEN", "feature/codecommit", "main", false), `"destinationReference":"refs/heads/main",`, "", 1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHost(tc.responses)
			pr, err := h.FindPR(context.Background(), "feature/codecommit", "")
			if err == nil {
				t.Fatalf("FindPR() = %+v, want error", pr)
			}
		})
	}
}

func TestFindPRReadsConsoleRegionFromRepositoryARN(t *testing.T) {
	t.Parallel()

	scope := " --output json --no-cli-pager --no-cli-auto-prompt --profile " + testProfile
	fake := &fakeAWS{responses: map[string]awsTestResponse{
		"aws codecommit list-pull-requests --repository-name " + testRepo + " --pull-request-status OPEN" + scope: {stdout: `{"pullRequestIds":["4"]}`},
		"aws codecommit get-pull-request --pull-request-id 4" + scope:                                             {stdout: pullRequestJSON("4", "OPEN", "feature/codecommit", "main", false)},
		"aws codecommit get-repository --repository-name " + testRepo + scope:                                     {stdout: repositoryJSON("arn:aws:codecommit:eu-west-2:123456789012:Example-Payments-Client")},
	}}
	h := New(fake.cmdFactory(), func() bool { return true }, "", testProfile, testRepo)

	for range 2 {
		pr, err := h.FindPR(context.Background(), "feature/codecommit", "")
		if err != nil {
			t.Fatalf("FindPR() error = %v", err)
		}
		want := "https://eu-west-2.console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/4"
		if pr == nil || pr.URL != want {
			t.Fatalf("FindPR() = %+v, want URL %q", pr, want)
		}
	}
	reads := 0
	for _, key := range fake.keys() {
		if strings.Contains(key, "get-repository") {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("get-repository ran %d times, want the region resolved once", reads)
	}
}

func TestCreatePRPassesRequestDocument(t *testing.T) {
	t.Parallel()

	// A comma in the branch would split the AWS CLI's --targets shorthand, and
	// the body mixes non-ASCII text with a line that looks like an option.
	branch := "fix/a,b"
	body := "## Intent\n\n--- a/file\n😀 café\n"
	h, fake := newTestHost(map[string]awsTestResponse{
		awsCmd("create-pull-request --cli-input-json file://<request>"): {stdout: pullRequestJSON("15", "OPEN", branch, "main", false)},
	})

	pr, err := h.CreatePR(context.Background(), branch, "main", scm.PRContent{Title: "feat: add CodeCommit", Body: body})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	want := scm.PR{Number: "15", URL: testPRURL + "15", BaseBranch: "main"}
	if pr == nil || *pr != want {
		t.Fatalf("CreatePR() = %+v, want %+v", pr, want)
	}

	call := fake.calls[0]
	for i := 0; i < len(call.input); i++ {
		if call.input[i] >= utf8.RuneSelf {
			t.Fatalf("request document has non-ASCII byte at %d: %q", i, call.input)
		}
	}
	var input createPullRequestInput
	if err := json.Unmarshal([]byte(call.input), &input); err != nil {
		t.Fatalf("decode request document %q: %v", call.input, err)
	}
	wantTargets := []targetInput{{RepositoryName: testRepo, SourceReference: branch, DestinationReference: "main"}}
	if input.Title != "feat: add CodeCommit" || input.Description != body || len(input.Targets) != 1 || input.Targets[0] != wantTargets[0] {
		t.Fatalf("request document = %+v, want title, exact body, and target %+v", input, wantTargets[0])
	}
	if _, err := os.Stat(call.requestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request file %q still exists after the call (stat err = %v)", call.requestPath, err)
	}
}

func TestCreatePRClampsToCodeCommitLimits(t *testing.T) {
	t.Parallel()

	h, fake := newTestHost(map[string]awsTestResponse{
		awsCmd("create-pull-request --cli-input-json file://<request>"): {stdout: pullRequestJSON("16", "OPEN", "feature", "main", false)},
	})
	title := strings.Repeat("😀", 100)
	body := strings.Repeat("b", 12000)
	if _, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: title, Body: body}); err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	var input createPullRequestInput
	if err := json.Unmarshal([]byte(fake.calls[0].input), &input); err != nil {
		t.Fatal(err)
	}
	if n := scm.PRBodyLen(input.Title); n > maxTitleChars || !strings.HasSuffix(input.Title, "…") || !utf8.ValidString(input.Title) {
		t.Fatalf("title = %q (%d units), want a valid ellipsized title within %d", input.Title, n, maxTitleChars)
	}
	if n := scm.PRBodyLen(input.Description); n > scm.MaxPRBodyChars(scm.ProviderCodeCommit) {
		t.Fatalf("description length = %d, want at most %d", n, scm.MaxPRBodyChars(scm.ProviderCodeCommit))
	}
}

func TestUpdatePRWritesTitleThenDescription(t *testing.T) {
	t.Parallel()

	responses := map[string]awsTestResponse{
		awsCmd("update-pull-request-description --cli-input-json file://<request>"): {stdout: pullRequestJSON("15", "OPEN", "feature", "main", false)},
		awsCmd("update-pull-request-title --cli-input-json file://<request>"):       {stdout: pullRequestJSON("15", "OPEN", "feature", "main", false)},
	}
	body := "# Updated\n\n😀 café\n"

	h, fake := newTestHost(responses)
	pr := &scm.PR{URL: testPRURL + "15"}
	got, err := h.UpdatePR(context.Background(), pr, scm.PRContent{Title: "feat: retitled", Body: body})
	if err != nil || got != pr {
		t.Fatalf("UpdatePR() = (%+v, %v), want the same PR", got, err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("UpdatePR() made %d calls, want title then description", len(fake.calls))
	}
	var title updateTitleInput
	if err := json.Unmarshal([]byte(fake.calls[0].input), &title); err != nil || title != (updateTitleInput{PullRequestID: "15", Title: "feat: retitled"}) {
		t.Fatalf("title request = %+v (%v)", title, err)
	}
	var description updateDescriptionInput
	if err := json.Unmarshal([]byte(fake.calls[1].input), &description); err != nil || description != (updateDescriptionInput{PullRequestID: "15", Description: body}) {
		t.Fatalf("description request = %+v (%v)", description, err)
	}

	h, fake = newTestHost(responses)
	if _, err := h.UpdatePR(context.Background(), &scm.PR{Number: "15"}, scm.PRContent{Body: body}); err != nil {
		t.Fatalf("UpdatePR(body only) error = %v", err)
	}
	if got := fake.keys(); len(got) != 1 || !strings.Contains(got[0], "update-pull-request-description") {
		t.Fatalf("UpdatePR(body only) commands = %q, want only the description update", got)
	}
}

func TestUpdatePRTitleFailureLeavesDescriptionUntouched(t *testing.T) {
	t.Parallel()

	h, fake := newTestHost(map[string]awsTestResponse{
		awsCmd("update-pull-request-title --cli-input-json file://<request>"): {stderr: "AccessDeniedException", code: 254},
	})
	_, err := h.UpdatePR(context.Background(), &scm.PR{Number: "15"}, scm.PRContent{Title: "feat: retitled", Body: "managed body"})
	if err == nil || !strings.Contains(err.Error(), "update-pull-request-title") {
		t.Fatalf("UpdatePR() error = %v, want title update failure", err)
	}
	if got := fake.keys(); len(got) != 1 || !strings.Contains(got[0], "update-pull-request-title") {
		t.Fatalf("UpdatePR() commands = %q, want only the title update", got)
	}
}

func TestUpdatePRRequiresPullRequestID(t *testing.T) {
	t.Parallel()

	h, fake := newTestHost(nil)
	if _, err := h.UpdatePR(context.Background(), &scm.PR{}, scm.PRContent{Body: "body"}); err == nil {
		t.Fatal("UpdatePR() error = nil, want missing PR id")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("UpdatePR() ran %d commands without a PR id", len(fake.calls))
	}
}

func TestGetPRStateNormalizesLifecycle(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status string
		merged bool
		want   scm.PRState
	}{
		{status: "OPEN", want: scm.PRStateOpen},
		{status: "CLOSED", merged: true, want: scm.PRStateMerged},
		{status: "CLOSED", want: scm.PRStateClosed},
	} {
		h, _ := newTestHost(map[string]awsTestResponse{
			awsCmd("get-pull-request --pull-request-id 21"): {stdout: pullRequestJSON("21", tc.status, "feature", "main", tc.merged)},
		})
		got, err := h.GetPRState(context.Background(), &scm.PR{Number: "21"})
		if err != nil || got != tc.want {
			t.Fatalf("GetPRState(%s, merged=%v) = (%q, %v), want %q", tc.status, tc.merged, got, err, tc.want)
		}
	}
}

func TestGetPRBaseBranchReadsLiveDestination(t *testing.T) {
	t.Parallel()

	h, _ := newTestHost(map[string]awsTestResponse{
		awsCmd("get-pull-request --pull-request-id 21"): {stdout: pullRequestJSON("21", "OPEN", "feature", "release", false)},
	})
	base, err := h.GetPRBaseBranch(context.Background(), &scm.PR{Number: "21"})
	if err != nil || base != "release" {
		t.Fatalf("GetPRBaseBranch() = (%q, %v), want (release, nil)", base, err)
	}
}

func TestGetMergedProofBindsMergedStateToExpectedHead(t *testing.T) {
	t.Parallel()

	t.Run("matching merged head", func(t *testing.T) {
		h, _ := newTestHost(map[string]awsTestResponse{
			awsCmd("get-pull-request --pull-request-id 21"): {stdout: pullRequestJSON("21", "CLOSED", "feature", "main", true)},
		})
		proof, err := h.GetMergedProof(context.Background(), &scm.PR{Number: "21", URL: testPRURL + "21"}, testHeadSHA)
		want := scm.MergedProof{Merged: true, Number: "21", URL: testPRURL + "21", HeadSHA: testHeadSHA, MergeCommitSHA: testMergeSHA}
		if err != nil || proof != want {
			t.Fatalf("GetMergedProof() = (%+v, %v), want (%+v, nil)", proof, err, want)
		}
	})

	t.Run("mismatched merged head", func(t *testing.T) {
		h, _ := newTestHost(map[string]awsTestResponse{
			awsCmd("get-pull-request --pull-request-id 21"): {stdout: pullRequestJSON("21", "CLOSED", "feature", "main", true)},
		})
		_, err := h.GetMergedProof(context.Background(), &scm.PR{Number: "21", URL: testPRURL + "21"}, "cccccccccccccccccccccccccccccccccccccccc")
		if !errors.Is(err, scm.ErrHeadChanged) {
			t.Fatalf("GetMergedProof() error = %v, want ErrHeadChanged", err)
		}
	})

	t.Run("matching unmerged head", func(t *testing.T) {
		h, _ := newTestHost(map[string]awsTestResponse{
			awsCmd("get-pull-request --pull-request-id 21"): {stdout: pullRequestJSON("21", "OPEN", "feature", "main", false)},
		})
		proof, err := h.GetMergedProof(context.Background(), &scm.PR{Number: "21", URL: testPRURL + "21"}, testHeadSHA)
		if err != nil || proof.Merged || proof.HeadSHA != testHeadSHA || proof.Number != "21" || proof.URL != testPRURL+"21" {
			t.Fatalf("GetMergedProof() = (%+v, %v), want matching unmerged proof", proof, err)
		}
	})
}

func TestOptionalOperationsReportNoChecksAndUnsupported(t *testing.T) {
	t.Parallel()

	h, fake := newTestHost(nil)
	pr := &scm.PR{Number: "21"}
	if checks, err := h.GetChecks(context.Background(), pr); err != nil || len(checks) != 0 {
		t.Fatalf("GetChecks() = (%v, %v), want no checks and no error", checks, err)
	}
	if _, err := h.GetMergeableState(context.Background(), pr); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("GetMergeableState() error = %v, want ErrUnsupported", err)
	}
	if _, err := h.FetchFailedCheckLogs(context.Background(), pr, "feature", "", nil); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("FetchFailedCheckLogs() error = %v, want ErrUnsupported", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("optional operations ran %d commands", len(fake.calls))
	}
}

type awsTestResponse struct {
	stdout string
	stderr string
	code   int
}

// awsCall records one aws invocation. A file:// request argument appears in
// key as "file://<request>" so response keys stay stable, and the request
// document it referenced is read while the command runs, because the file is
// removed once the call returns.
type awsCall struct {
	key         string
	input       string
	requestPath string
}

type fakeAWS struct {
	responses map[string]awsTestResponse
	calls     []awsCall
}

func (f *fakeAWS) keys() []string {
	keys := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		keys = append(keys, call.key)
	}
	return keys
}

func (f *fakeAWS) cmdFactory() CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		var call awsCall
		keyArgs := make([]string, 0, len(args))
		for _, arg := range args {
			if path, ok := strings.CutPrefix(arg, "file://"); ok {
				call.requestPath = path
				if data, err := os.ReadFile(path); err == nil {
					call.input = string(data)
				}
				arg = "file://<request>"
			}
			keyArgs = append(keyArgs, arg)
		}
		call.key = strings.TrimSpace(name + " " + strings.Join(keyArgs, " "))
		f.calls = append(f.calls, call)
		response, ok := f.responses[call.key]
		if !ok {
			response = awsTestResponse{stderr: "unexpected command: " + call.key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestCodeCommitHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"CODECOMMIT_TEST_HELPER=1",
			"CODECOMMIT_TEST_STDOUT="+response.stdout,
			"CODECOMMIT_TEST_STDERR="+response.stderr,
			fmt.Sprintf("CODECOMMIT_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestCodeCommitHelperProcess(t *testing.T) {
	if os.Getenv("CODECOMMIT_TEST_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stdout, os.Getenv("CODECOMMIT_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("CODECOMMIT_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("CODECOMMIT_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
