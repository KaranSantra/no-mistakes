// Package codecommit implements scm.Host backed by the AWS CLI's codecommit
// commands.
//
// No AWS credentials pass through no-mistakes. Every command carries the
// --profile the repository's remote URL names and the --region when it names
// one. Region resolution may remain in the selected profile, but an unnamed
// profile is refused so Git and PR operations cannot silently use different
// AWS identities.
//
// CodeCommit has neither a check/status API for pull requests nor a stored
// pull request merge status, so GetChecks reports no checks and mergeability
// is declined.
package codecommit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// maxTitleChars is CodeCommit's pull request title limit.
const maxTitleChars = 150

// outputJSON runs cmd and returns its stdout alone, leaving stderr out of the
// payload so AWS CLI warnings (SSO token-refresh notices, deprecation messages)
// cannot corrupt the bytes a caller json.Unmarshal's. On failure it surfaces
// the separately-captured stderr in the error.
func outputJSON(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(ee.Stderr)), err)
		}
		return nil, err
	}
	return out, nil
}

// clampDescription truncates body to CodeCommit's PR-description cap. The
// pipeline already budgets the body to fit (shedding whole sections), so this
// is the connector-level backstop that guarantees CodeCommit never sees an
// over-length description, no matter how the body was produced.
func clampDescription(body string) string {
	return scm.ClampPRBody(body, scm.MaxPRBodyChars(scm.ProviderCodeCommit))
}

// clampTitle truncates title to CodeCommit's title limit, measured like
// scm.PRBodyLen, ending a cut title with an ellipsis. Titles are drafted with
// no provider limit, and CodeCommit rejects an over-length one outright.
func clampTitle(title string) string {
	if scm.PRBodyLen(title) <= maxTitleChars {
		return title
	}
	var b strings.Builder
	used := 0
	for _, r := range title {
		w := scm.PRBodyLen(string(r))
		if used+w > maxTitleChars-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteString("…")
	return b.String()
}

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to AWS CodeCommit through the AWS CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	region       string // AWS region named by the remote; empty defers to the AWS CLI
	profile      string // AWS CLI profile named by the remote
	repo         string // repository name

	mu               sync.Mutex
	repositoryRegion string // region from the repository ARN, recorded by getRepository
}

// New builds a Host. cliAvailable reports whether the aws binary is resolvable
// on the caller's PATH. region and profile are the values the remote URL names
// (see ParseRemote); region may be empty and resolve through the selected
// profile. repo names the repository every command is scoped to.
func New(cmd CmdFactory, cliAvailable func() bool, region, profile, repo string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		region:       strings.TrimSpace(region),
		profile:      strings.TrimSpace(profile),
		repo:         strings.TrimSpace(repo),
	}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderCodeCommit }

// Capabilities reports the CodeCommit feature matrix. Mergeability is not wired
// up: CodeCommit keeps no merge status on a pull request, only an on-demand
// get-merge-conflicts evaluation per merge strategy. Failed-check logs do not
// apply because GetChecks reports no checks.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: false, FailedCheckLogs: false, MergedProof: true}
}

// globalArgs pins every command to JSON on stdout, whatever output format or
// pager the user's AWS CLI configuration selects, and to the profile and region
// the remote URL names.
func (h *Host) globalArgs() []string {
	args := []string{"--output", "json", "--no-cli-pager"}
	if h.profile != "" {
		args = append(args, "--profile", h.profile)
	}
	if h.region != "" {
		args = append(args, "--region", h.region)
	}
	return args
}

// run executes `aws codecommit <args>` scoped by globalArgs and returns its
// JSON stdout.
func (h *Host) run(ctx context.Context, args ...string) ([]byte, error) {
	if h.profile == "" {
		return nil, errors.New("AWS CodeCommit requires an explicit AWS profile")
	}
	argv := append([]string{"codecommit"}, args...)
	argv = append(argv, h.globalArgs()...)
	return outputJSON(h.cmd(ctx, "aws", argv...))
}

// runWithInput runs an aws codecommit command whose parameters are supplied as
// a request document through `--cli-input-json file://<path>` instead of as
// individual flags, and returns the command's JSON stdout. The file is always
// removed afterward.
//
// Why a request file: `create-pull-request --targets` otherwise takes the AWS
// CLI's shorthand syntax, which splits values on commas, so a branch named
// "fix/a,b" would be rejected as a list; and an escaped maximum-length
// description can exceed Windows' 32,767-character command-line limit. The
// document is ASCII-only (marshalASCIIJSON) because the AWS CLI decodes local
// files with the platform's locale encoding unless AWS_CLI_FILE_ENCODING is
// set, which would garble non-ASCII text on a non-UTF-8 locale.
func (h *Host) runWithInput(ctx context.Context, command string, input any) ([]byte, error) {
	data, err := marshalASCIIJSON(input)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	f, err := os.CreateTemp("", "nm-codecommit-*.json")
	if err != nil {
		return nil, fmt.Errorf("create request temp file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, fmt.Errorf("write request temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close request temp file: %w", err)
	}
	return h.run(ctx, command, "--cli-input-json", "file://"+path)
}

// marshalASCIIJSON encodes v as JSON with every non-ASCII character escaped as
// \uXXXX (a UTF-16 surrogate pair beyond the Basic Multilingual Plane).
// Non-ASCII bytes only ever occur inside JSON strings, so the escaped document
// is equivalent and decodes identically under any ASCII-compatible encoding.
func marshalASCIIJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, r := range string(raw) {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
			continue
		}
		for _, unit := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, `\u%04x`, unit)
		}
	}
	return b.Bytes(), nil
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("aws CLI is not installed")
	}
	// Auth probe: reading this repository's metadata exercises the same
	// profile, region, and credentials as every later command, so an expired
	// SSO session or a profile without access to the repository is reported
	// before publication starts.
	if err := h.getRepository(ctx); err != nil {
		return fmt.Errorf("aws CLI cannot read CodeCommit repository %q%s (for an expired SSO session, run `aws sso login`): %w", h.repo, h.scopeDescription(), err)
	}
	return nil
}

func (h *Host) scopeDescription() string {
	var parts []string
	if h.profile != "" {
		parts = append(parts, "profile "+h.profile)
	}
	if h.region != "" {
		parts = append(parts, "region "+h.region)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	out, err := h.run(ctx, "list-pull-requests", "--repository-name", h.repo, "--pull-request-status", "OPEN")
	if err != nil {
		return nil, fmt.Errorf("aws codecommit list-pull-requests: %w", err)
	}
	var list pullRequestList
	if err := json.Unmarshal(bytes.TrimSpace(out), &list); err != nil {
		return nil, fmt.Errorf("aws codecommit list-pull-requests: parse response: %w", err)
	}
	if list.PullRequestIDs == nil {
		return nil, errors.New("aws codecommit list-pull-requests: parse response: expected pullRequestIds array")
	}
	ids, err := newestFirst(list.PullRequestIDs)
	if err != nil {
		return nil, fmt.Errorf("aws codecommit list-pull-requests: parse response: %w", err)
	}
	// list-pull-requests returns only IDs and cannot filter by branch, so open
	// pull requests are read one at a time until one matches: one aws process
	// per open pull request in the worst case. CodeCommit does not stop two
	// open pull requests from sharing a source branch; reading the highest ID
	// first makes the most recently created one win, and usually matches the
	// run's own pull request after a single read.
	base = strings.TrimSpace(base)
	for _, id := range ids {
		got, err := h.getPullRequest(ctx, id)
		if err != nil {
			return nil, err
		}
		target := got.Targets[0]
		// A pull request closed since the listing no longer counts.
		if !strings.EqualFold(got.PullRequestStatus, "OPEN") || branchName(target.SourceReference) != branch {
			continue
		}
		if base != "" && branchName(target.DestinationReference) != base {
			continue
		}
		return h.toPR(ctx, got)
	}
	return nil, nil
}

// newestFirst validates listed pull request IDs and orders them highest first.
func newestFirst(ids []string) ([]string, error) {
	numbers := make(map[string]int, len(ids))
	for i, id := range ids {
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("entry %d: invalid pullRequestId %q", i, id)
		}
		numbers[id] = n
	}
	sorted := append([]string(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return numbers[sorted[i]] > numbers[sorted[j]] })
	return sorted, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	out, err := h.runWithInput(ctx, "create-pull-request", createPullRequestInput{
		Title:       clampTitle(content.Title),
		Description: clampDescription(content.Body),
		Targets: []targetInput{{
			RepositoryName:       h.repo,
			SourceReference:      branch,
			DestinationReference: base,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("aws codecommit create-pull-request: %w", err)
	}
	got, err := h.parsePullRequest(out, "")
	if err != nil {
		return nil, fmt.Errorf("aws codecommit create-pull-request: parse response: %w", err)
	}
	return h.toPR(ctx, got)
}

// UpdatePR replaces the description and, when content carries one, the title.
// CodeCommit has no combined update, so these are separate commands and a
// failed title update leaves the new description in place; the error still
// fails the call, and the pipeline's next publication rewrites both.
func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := prID(pr)
	if id == "" {
		return nil, errors.New("aws codecommit update-pull-request-description: missing PR id")
	}
	if _, err := h.runWithInput(ctx, "update-pull-request-description", updateDescriptionInput{
		PullRequestID: id,
		Description:   clampDescription(content.Body),
	}); err != nil {
		return nil, fmt.Errorf("aws codecommit update-pull-request-description: %w", err)
	}
	if content.Title != "" {
		if _, err := h.runWithInput(ctx, "update-pull-request-title", updateTitleInput{
			PullRequestID: id,
			Title:         clampTitle(content.Title),
		}); err != nil {
			return nil, fmt.Errorf("aws codecommit update-pull-request-title: %w", err)
		}
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizePRState(got), nil
}

func (h *Host) GetMergedProof(ctx context.Context, pr *scm.PR, expectedHead string) (scm.MergedProof, error) {
	expectedHead = strings.TrimSpace(expectedHead)
	if expectedHead == "" {
		return scm.MergedProof{}, errors.New("AWS CodeCommit merged proof requires an expected head SHA")
	}
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return scm.MergedProof{}, err
	}
	target := got.Targets[0]
	head := strings.TrimSpace(target.SourceCommit)
	if head == "" {
		return scm.MergedProof{}, errors.New("aws codecommit get-pull-request: missing sourceCommit")
	}
	if head != expectedHead {
		return scm.MergedProof{}, fmt.Errorf("%w: AWS CodeCommit reported %s, expected %s", scm.ErrHeadChanged, head, expectedHead)
	}
	canonical, err := h.toPR(ctx, got)
	if err != nil {
		return scm.MergedProof{}, err
	}
	return scm.MergedProof{
		Merged:         target.MergeMetadata.IsMerged,
		Number:         canonical.Number,
		URL:            canonical.URL,
		HeadSHA:        head,
		MergeCommitSHA: strings.TrimSpace(target.MergeMetadata.MergeCommitID),
	}, nil
}

// GetChecks reports no checks: CodeCommit has no check or status API for pull
// requests. An empty list (rather than ErrUnsupported, which the CI step would
// treat as a failed poll) leaves the CI step watching PR state like any
// repository with no registered checks, and a trusted `no_ci: true`
// declaration establishes readiness.
func (h *Host) GetChecks(_ context.Context, _ *scm.PR) ([]scm.Check, error) {
	return nil, nil
}

// GetMergeableState is not implemented for CodeCommit; callers gate on
// Capabilities().MergeableState (false) and skip it.
func (h *Host) GetMergeableState(_ context.Context, _ *scm.PR) (scm.MergeableState, error) {
	return "", scm.ErrUnsupported
}

// FetchFailedCheckLogs is not implemented for CodeCommit; callers gate on
// Capabilities().FailedCheckLogs (false) and skip it.
func (h *Host) FetchFailedCheckLogs(_ context.Context, _ *scm.PR, _ string, _ string, _ []string) (string, error) {
	return "", scm.ErrUnsupported
}

func (h *Host) showPR(ctx context.Context, pr *scm.PR) (*pullRequest, error) {
	id := prID(pr)
	if id == "" {
		return nil, errors.New("aws codecommit get-pull-request: missing PR id")
	}
	return h.getPullRequest(ctx, id)
}

func (h *Host) getPullRequest(ctx context.Context, id string) (*pullRequest, error) {
	out, err := h.run(ctx, "get-pull-request", "--pull-request-id", id)
	if err != nil {
		return nil, fmt.Errorf("aws codecommit get-pull-request: %w", err)
	}
	got, err := h.parsePullRequest(out, id)
	if err != nil {
		return nil, fmt.Errorf("aws codecommit get-pull-request: parse response: %w", err)
	}
	return got, nil
}

// parsePullRequest decodes a pull request response and proves it describes a
// pull request in this repository: a positive ID (equal to wantID when one is
// given) and a single target in h.repo. CodeCommit pull request IDs are unique
// per account and region rather than per repository, so the repository check
// is what stops a stale ID from reading or matching another repository's pull
// request.
func (h *Host) parsePullRequest(out []byte, wantID string) (*pullRequest, error) {
	var resp pullRequestResponse
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return nil, err
	}
	got := resp.PullRequest
	if got == nil {
		return nil, errors.New("missing pullRequest")
	}
	if n, err := strconv.Atoi(got.PullRequestID); err != nil || n <= 0 {
		return nil, errors.New("missing positive pullRequestId")
	}
	if wantID != "" && got.PullRequestID != wantID {
		return nil, fmt.Errorf("pullRequestId %q does not match requested pull request %q", got.PullRequestID, wantID)
	}
	if len(got.Targets) != 1 {
		return nil, fmt.Errorf("expected one pull request target, got %d", len(got.Targets))
	}
	status := strings.ToUpper(strings.TrimSpace(got.PullRequestStatus))
	if status != "OPEN" && status != "CLOSED" {
		return nil, fmt.Errorf("unknown pullRequestStatus %q", got.PullRequestStatus)
	}
	target := got.Targets[0]
	if name := target.RepositoryName; name != h.repo {
		return nil, fmt.Errorf("repository %q does not match configured repository %q", name, h.repo)
	}
	if strings.TrimSpace(target.SourceReference) == "" {
		return nil, errors.New("missing sourceReference")
	}
	if strings.TrimSpace(target.DestinationReference) == "" {
		return nil, errors.New("missing destinationReference")
	}
	return got, nil
}

// getRepository reads this repository's metadata and records the region in its
// ARN, which is the region the AWS CLI actually resolved for these commands.
func (h *Host) getRepository(ctx context.Context) error {
	out, err := h.run(ctx, "get-repository", "--repository-name", h.repo)
	if err != nil {
		return fmt.Errorf("aws codecommit get-repository: %w", err)
	}
	var resp repositoryResponse
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return fmt.Errorf("aws codecommit get-repository: parse response: %w", err)
	}
	meta := resp.RepositoryMetadata
	if meta == nil || meta.RepositoryName != h.repo {
		return errors.New("aws codecommit get-repository: parse response: repository metadata does not match configured repository")
	}
	region, ok := arnRegion(meta.ARN)
	if !ok {
		return errors.New("aws codecommit get-repository: parse response: missing valid repository Arn")
	}
	h.mu.Lock()
	h.repositoryRegion = region
	h.mu.Unlock()
	return nil
}

// consoleRegion returns the region for browsable pull request URLs: the one the
// remote names or, for a codecommit://[profile@]repository remote that leaves
// the region to the AWS CLI's configuration, the one in the repository's ARN.
func (h *Host) consoleRegion(ctx context.Context) (string, error) {
	if h.region != "" {
		return h.region, nil
	}
	h.mu.Lock()
	region := h.repositoryRegion
	h.mu.Unlock()
	if region != "" {
		return region, nil
	}
	if err := h.getRepository(ctx); err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.repositoryRegion, nil
}

func (h *Host) toPR(ctx context.Context, raw *pullRequest) (*scm.PR, error) {
	region, err := h.consoleRegion(ctx)
	if err != nil {
		return nil, err
	}
	return &scm.PR{
		Number:     raw.PullRequestID,
		URL:        webPRURL(region, h.repo, raw.PullRequestID),
		BaseBranch: branchName(raw.Targets[0].DestinationReference),
	}, nil
}

func prID(pr *scm.PR) string {
	if pr == nil {
		return ""
	}
	if id := strings.TrimSpace(pr.Number); id != "" {
		return id
	}
	if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
		return num
	}
	return ""
}
