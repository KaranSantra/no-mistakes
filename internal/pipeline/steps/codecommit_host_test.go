package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const codeCommitTestProfile = "AWSAdministratorAccess-123456789012"

func TestBuildHost_CodeCommit(t *testing.T) {
	for _, remote := range []string{
		"codecommit::us-east-1://" + codeCommitTestProfile + "@Example-Payments-Client",
		"codecommit://" + codeCommitTestProfile + "@Example-Payments-Client",
	} {
		t.Run(remote, func(t *testing.T) {
			sctx := &pipeline.StepContext{
				Ctx:     context.Background(),
				WorkDir: t.TempDir(),
				Run:     &db.Run{Branch: "feature/codecommit"},
				Repo: &db.Repo{
					UpstreamURL:   remote,
					DefaultBranch: "main",
				},
			}
			if got := resolvedProvider(sctx); got != scm.ProviderCodeCommit {
				t.Fatalf("resolvedProvider() = %q, want %q", got, scm.ProviderCodeCommit)
			}
			host, reason := buildHost(sctx, scm.ProviderCodeCommit)
			if host == nil || reason != "" {
				t.Fatalf("buildHost() = (%v, %q), want CodeCommit host", host, reason)
			}
			if host.Provider() != scm.ProviderCodeCommit {
				t.Fatalf("Provider() = %q, want %q", host.Provider(), scm.ProviderCodeCommit)
			}
		})
	}
}

// URL redaction rewrites the profile in a codecommit://<profile>@<repository>
// remote before the repository record is stored. The profile selects the AWS
// credentials, so the host must take it from the worktree's origin instead.
func TestBuildHost_CodeCommitUsesProfileFromWorktreeOrigin(t *testing.T) {
	workDir := t.TempDir()
	origin := "codecommit://" + codeCommitTestProfile + "@Example-Payments-Client"
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		if out, err := exec.Command(testGitExecutable, append([]string{"-C", workDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	redacted := safeurl.Redact(origin)
	if strings.Contains(redacted, codeCommitTestProfile) {
		t.Fatalf("safeurl.Redact(%q) = %q, want the profile redacted", origin, redacted)
	}

	binDir := fakeCLIBinDir(t)
	logFile := filepath.Join(t.TempDir(), "aws.log")
	linkTestBinary(t, binDir, "aws")
	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: workDir,
		Run:     &db.Run{Branch: "feature/codecommit"},
		Repo:    &db.Repo{UpstreamURL: redacted, DefaultBranch: "main"},
		Env:     fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "record-success", "FAKE_CLI_LOG": logFile}),
	}

	if got := resolvedProvider(sctx); got != scm.ProviderCodeCommit {
		t.Fatalf("resolvedProvider() = %q, want %q", got, scm.ProviderCodeCommit)
	}
	host, reason := buildHost(sctx, scm.ProviderCodeCommit)
	if host == nil || reason != "" {
		t.Fatalf("buildHost() = (%v, %q), want CodeCommit host", host, reason)
	}
	// The recording stub prints no repository metadata, so the probe fails
	// after the command has run with its full scope.
	if err := host.Available(sctx.Ctx); err == nil {
		t.Fatal("Available() error = nil, want unreadable repository from the recording stub")
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read aws log: %v", err)
	}
	want := "codecommit get-repository --repository-name Example-Payments-Client --output json --no-cli-pager --profile " + codeCommitTestProfile
	if !strings.Contains(string(logged), want) {
		t.Fatalf("aws invocations = %q, want %q", logged, want)
	}
}

func TestBuildHost_CodeCommitRefusesProfilelessRemoteForms(t *testing.T) {
	prURL := "https://eu-west-2.console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/42"
	for _, tc := range []struct {
		name   string
		remote string
		prURL  *string
	}{
		{name: "HTTPS endpoint", remote: "https://git-codecommit.us-east-1.amazonaws.com/v1/repos/Example-Payments-Client"},
		{name: "SSH endpoint", remote: "ssh://SSHKEYID@git-codecommit.us-east-1.amazonaws.com/v1/repos/Example-Payments-Client"},
		{name: "helper without profile", remote: "codecommit::us-east-1://Example-Payments-Client"},
		{name: "console PR URL fallback", prURL: &prURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx := &pipeline.StepContext{
				Ctx:     context.Background(),
				WorkDir: t.TempDir(),
				Run:     &db.Run{Branch: "feature/codecommit", PRURL: tc.prURL},
				Repo:    &db.Repo{UpstreamURL: tc.remote, DefaultBranch: "main"},
			}
			if got := resolvedProvider(sctx); got != scm.ProviderCodeCommit {
				t.Fatalf("resolvedProvider() = %q, want %q", got, scm.ProviderCodeCommit)
			}
			host, reason := buildHost(sctx, scm.ProviderCodeCommit)
			if host != nil || reason != codeCommitProfileRequiredReason {
				t.Fatalf("buildHost() = (%v, %q), want (nil, %q)", host, reason, codeCommitProfileRequiredReason)
			}
		})
	}
}

func TestBuildHost_CodeCommitSkipsUnroutableRepositories(t *testing.T) {
	for _, tc := range []struct {
		name       string
		repo       *db.Repo
		wantReason string
	}{
		{
			name: "fork routing",
			repo: &db.Repo{
				UpstreamURL: "codecommit::us-east-1://" + codeCommitTestProfile + "@Example-Payments-Client",
				ForkURL:     "codecommit::us-east-1://" + codeCommitTestProfile + "@Example-Payments-Client",
			},
			wantReason: "fork",
		},
		{
			// The worktree has no origin to recover the profile from.
			name:       "redacted profile",
			repo:       &db.Repo{UpstreamURL: "codecommit://" + safeurl.RedactedUserinfo + "@Example-Payments-Client"},
			wantReason: "explicit AWS profile",
		},
		{
			name:       "unparseable remote",
			repo:       &db.Repo{UpstreamURL: "codecommit://"},
			wantReason: "could not resolve",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: t.TempDir(), Run: &db.Run{}, Repo: tc.repo}
			host, reason := buildHost(sctx, scm.ProviderCodeCommit)
			if host != nil || !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("buildHost() = (%v, %q), want skip reason containing %q", host, reason, tc.wantReason)
			}
		})
	}
}
