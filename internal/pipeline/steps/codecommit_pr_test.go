package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestPRStep_CreatesCodeCommitPullRequest(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "aws")
	commandLog := filepath.Join(t.TempDir(), "aws.log")
	requestLog := filepath.Join(t.TempDir(), "request.json")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "aws-codecommit",
		"FAKE_CLI_LOG":               commandLog,
		"FAKE_CLI_REQUEST_LOG":       requestLog,
		"FAKE_CLI_CODECOMMIT_REPO":   "Example-Payments-Client",
		"FAKE_CLI_CODECOMMIT_REGION": "us-east-1",
	})

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: add CodeCommit support","body":"## What Changed\n\n- Route pipeline pull requests through AWS CodeCommit."}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "codecommit::us-east-1://AWSAdministratorAccess-123456789012@Example-Payments-Client"
	// Run start already verified/refreshed this clone target before the PR step.
	sctx.Repo.URLsVerified = true
	var pipelineLog []string
	sctx.Log = func(line string) {
		pipelineLog = append(pipelineLog, line)
	}

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "https://us-east-1.console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/15"
	if outcome.Skipped || outcome.PRURL != wantURL {
		t.Fatalf("PR outcome = %+v, want published CodeCommit URL %q", outcome, wantURL)
	}
	joinedPipelineLog := strings.Join(pipelineLog, "\n")
	if strings.Contains(joinedPipelineLog, "provider unknown") || !strings.Contains(joinedPipelineLog, "created pull request: "+wantURL) {
		t.Fatalf("pipeline log:\n%s", joinedPipelineLog)
	}

	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	commandText := string(commands)
	for _, want := range []string{
		"codecommit get-repository --repository-name Example-Payments-Client",
		"codecommit list-pull-requests --repository-name Example-Payments-Client --pull-request-status OPEN",
		"codecommit create-pull-request --cli-input-json file://",
		"--output json --no-cli-pager --no-cli-auto-prompt --profile AWSAdministratorAccess-123456789012 --region us-east-1",
	} {
		if !strings.Contains(commandText, want) {
			t.Fatalf("AWS CLI transcript missing %q:\n%s", want, commandText)
		}
	}

	request, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Targets     []struct {
			RepositoryName       string `json:"repositoryName"`
			SourceReference      string `json:"sourceReference"`
			DestinationReference string `json:"destinationReference"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(request, &input); err != nil {
		t.Fatal(err)
	}
	if input.Title != "feat: add CodeCommit support" || len(input.Targets) != 1 || input.Targets[0].RepositoryName != "Example-Payments-Client" || input.Targets[0].SourceReference != "feature" || input.Targets[0].DestinationReference != "main" {
		t.Fatalf("CodeCommit request = %+v", input)
	}
	if !strings.Contains(input.Description, "Route pipeline pull requests through AWS CodeCommit.") {
		t.Fatalf("CodeCommit description:\n%s", input.Description)
	}

	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PRURL == nil || *stored.PRURL != wantURL {
		t.Fatalf("stored PR URL = %v, want %q", stored.PRURL, wantURL)
	}

	normalizedCommands := normalizeCodeCommitRequestPaths(commandText)
	t.Logf("pipeline transcript:\n%s\n\nAWS CLI transcript:\n%s\npublished request:\n%s\n\nstored PR URL: %s", joinedPipelineLog, normalizedCommands, request, *stored.PRURL)
}

func normalizeCodeCommitRequestPaths(transcript string) string {
	lines := strings.Split(transcript, "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		for j, field := range fields {
			if strings.HasPrefix(field, "file://") {
				fields[j] = "file://<request>"
			}
		}
		lines[i] = strings.Join(fields, " ")
	}
	return strings.Join(lines, "\n")
}
