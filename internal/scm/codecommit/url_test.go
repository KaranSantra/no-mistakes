package codecommit

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestParseRemote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		in          string
		wantRegion  string
		wantProfile string
		wantRepo    string
		wantOK      bool
	}{
		{
			name:        "helper with region and profile",
			in:          "codecommit::us-east-1://AWSAdministratorAccess-123456789012@Example-Payments-Client",
			wantRegion:  "us-east-1",
			wantProfile: "AWSAdministratorAccess-123456789012",
			wantRepo:    "Example-Payments-Client",
			wantOK:      true,
		},
		{
			name:        "helper with profile reads region from the profile",
			in:          "codecommit://AWSAdministratorAccess-123456789012@Example-Payments-Client",
			wantProfile: "AWSAdministratorAccess-123456789012",
			wantRepo:    "Example-Payments-Client",
			wantOK:      true,
		},
		{
			name:       "helper with region and default profile",
			in:         "codecommit::eu-west-2://Example-Payments-Client",
			wantRegion: "eu-west-2",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{
			name:     "helper with neither region nor profile",
			in:       "codecommit://Example-Payments-Client",
			wantRepo: "Example-Payments-Client",
			wantOK:   true,
		},
		{
			name:       "https endpoint",
			in:         "https://git-codecommit.us-east-1.amazonaws.com/v1/repos/Example-Payments-Client",
			wantRegion: "us-east-1",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{
			name:       "https git credentials are not a profile",
			in:         "https://git-user@git-codecommit.us-east-1.amazonaws.com/v1/repos/Example-Payments-Client",
			wantRegion: "us-east-1",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{
			name:       "ssh endpoint with key id",
			in:         "ssh://SSHKEYID@git-codecommit.us-east-2.amazonaws.com/v1/repos/Example-Payments-Client",
			wantRegion: "us-east-2",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{
			name:       "scp-like ssh endpoint",
			in:         "git-codecommit.us-east-1.amazonaws.com:v1/repos/Example-Payments-Client",
			wantRegion: "us-east-1",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{
			name:       "fips endpoint",
			in:         "https://git-codecommit-fips.us-gov-west-1.amazonaws.com/v1/repos/Example-Payments-Client",
			wantRegion: "us-gov-west-1",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{
			name:       "console pull request url",
			in:         "https://us-east-1.console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/42",
			wantRegion: "us-east-1",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{
			name:       "global console url with region query",
			in:         "https://console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/42/details?region=eu-west-1",
			wantRegion: "eu-west-1",
			wantRepo:   "Example-Payments-Client",
			wantOK:     true,
		},
		{name: "empty", in: ""},
		{name: "github", in: "https://github.com/octo/widgets.git"},
		{name: "azure devops", in: "https://dev.azure.com/myorg/myproject/_git/myrepo"},
		{name: "helper scheme alone", in: "codecommit:"},
		{name: "helper region without address", in: "codecommit::us-east-1:/Example-Payments-Client"},
		{name: "helper invalid region", in: "codecommit::not a region://Example-Payments-Client"},
		{name: "helper empty profile", in: "codecommit://@Example-Payments-Client"},
		{name: "helper empty repository", in: "codecommit://AWSAdministratorAccess-123456789012@"},
		{name: "helper repository with path", in: "codecommit://Example-Payments-Client/extra"},
		{name: "helper invalid repository name", in: "codecommit://Example Payments Client"},
		{name: "http endpoint", in: "http://git-codecommit.us-east-1.amazonaws.com/v1/repos/Example-Payments-Client"},
		{name: "endpoint without repos path", in: "https://git-codecommit.us-east-1.amazonaws.com/v1/Example-Payments-Client"},
		{name: "lookalike endpoint host", in: "https://git-codecommit.us-east-1.amazonaws.com.example.test/v1/repos/Example-Payments-Client"},
		{name: "console url for another service", in: "https://us-east-1.console.aws.amazon.com/ec2/home"},
		{name: "console region from another partition", in: "https://us-gov-west-1.console.aws.amazon.com/codesuite/codecommit/repositories/Example-Payments-Client/pull-requests/42"},
		{name: "scp remote on an ssh host named codecommit", in: "codecommit:v1/repos/Example-Payments-Client"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			region, profile, repo, ok := ParseRemote(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseRemote(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if region != tc.wantRegion || profile != tc.wantProfile || repo != tc.wantRepo {
				t.Fatalf("ParseRemote(%q) = (%q, %q, %q), want (%q, %q, %q)", tc.in, region, profile, repo, tc.wantRegion, tc.wantProfile, tc.wantRepo)
			}
		})
	}
}

func TestResolveRemoteUsesSSHAliasHost(t *testing.T) {
	t.Parallel()

	for _, remote := range []string{
		"ssh://codecommit-work/v1/repos/Example-Payments-Client",
		"codecommit:v1/repos/Example-Payments-Client",
	} {
		region, profile, repo, ok := ResolveRemote(remote, "git-codecommit.eu-west-2.amazonaws.com")
		if !ok || region != "eu-west-2" || profile != "" || repo != "Example-Payments-Client" {
			t.Fatalf("ResolveRemote(%q) = (%q, %q, %q, %v), want (eu-west-2, \"\", Example-Payments-Client, true)", remote, region, profile, repo, ok)
		}
	}
	if _, _, _, ok := ResolveRemote("ssh://codecommit-work/v1/repos/Example-Payments-Client", "github.com"); ok {
		t.Fatal("ResolveRemote() accepted an alias that resolves to another host")
	}
}

// A run persists only the PR URL; resuming it re-derives the provider, the
// repository, and the PR number from that URL alone.
func TestWebPRURLRoundTripsThroughRunRecovery(t *testing.T) {
	t.Parallel()

	for _, region := range []string{"us-east-1", "us-gov-west-1"} {
		prURL := webPRURL(region, "Example-Payments-Client", "42")
		if got := scm.DetectProviderStaticContext(t.Context(), prURL); got != scm.ProviderCodeCommit {
			t.Fatalf("DetectProviderStaticContext(%q) = %q, want %q", prURL, got, scm.ProviderCodeCommit)
		}
		gotRegion, _, gotRepo, ok := ParseRemote(prURL)
		if !ok || gotRegion != region || gotRepo != "Example-Payments-Client" {
			t.Fatalf("ParseRemote(%q) = (%q, %q, %v), want (%q, Example-Payments-Client, true)", prURL, gotRegion, gotRepo, ok, region)
		}
		if num, err := scm.ExtractPRNumber(prURL); err != nil || num != "42" {
			t.Fatalf("ExtractPRNumber(%q) = (%q, %v), want 42", prURL, num, err)
		}
	}
}
