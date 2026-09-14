package steps

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const codeCommitTestProfile = "AWSAdministratorAccess-123456789012"

func TestBuildHost_CodeCommit(t *testing.T) {
	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Run:     &db.Run{Branch: "feature/codecommit"},
		Repo: &db.Repo{
			UpstreamURL:   "codecommit::us-east-1://" + codeCommitTestProfile + "@Example-Payments-Client",
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
}

func TestBuildHost_CodeCommitRefusesUnsupportedRemoteForms(t *testing.T) {
	prURL := "https://eu-west-2.console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/42"
	for _, tc := range []struct {
		name   string
		remote string
		prURL  *string
	}{
		{name: "HTTPS endpoint", remote: "https://git-codecommit.us-east-1.amazonaws.com/v1/repos/Example-Payments-Client"},
		{name: "SSH endpoint", remote: "ssh://SSHKEYID@git-codecommit.us-east-1.amazonaws.com/v1/repos/Example-Payments-Client"},
		{name: "hierarchical helper with profile", remote: "codecommit://" + codeCommitTestProfile + "@Example-Payments-Client"},
		{name: "hierarchical helper without profile", remote: "codecommit://Example-Payments-Client"},
		{name: "opaque helper without profile", remote: "codecommit::us-east-1://Example-Payments-Client"},
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

func TestBuildHost_CodeCommitRefusesForkRouting(t *testing.T) {
	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Run:     &db.Run{},
		Repo: &db.Repo{
			UpstreamURL: "codecommit::us-east-1://" + codeCommitTestProfile + "@Example-Payments-Client",
			ForkURL:     "codecommit::us-east-1://" + codeCommitTestProfile + "@Example-Payments-Client",
		},
	}
	host, reason := buildHost(sctx, scm.ProviderCodeCommit)
	if host != nil || reason != "fork PR routing for AWS CodeCommit is not implemented" {
		t.Fatalf("buildHost() = (%v, %q), want fork routing refusal", host, reason)
	}
}
