package codecommit

import (
	"net/url"
	"regexp"
	"strings"
)

// helperScheme prefixes every git-remote-codecommit remote URL.
const helperScheme = "codecommit:"

const UnsupportedRemoteReason = "AWS CodeCommit requires an explicit AWS profile; re-point the remote with `git remote set-url origin codecommit::<region>://<profile>@<repository>`"

// repositoryNamePattern is CodeCommit's documented repository-name constraint.
var repositoryNamePattern = regexp.MustCompile(`^[\w.-]{1,100}$`)

// regionPattern matches AWS region names such as us-east-1 or us-gov-west-1.
var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// ParseRemote extracts the AWS region, AWS CLI profile, and repository name
// from a CodeCommit git remote (or console pull-request) URL. It returns
// ok=false for any non-CodeCommit remote or when the repository cannot be
// determined.
//
// Recognized forms:
//
//	codecommit::{region}://[{profile}@]{repository}
//	codecommit://[{profile}@]{repository}
//	https://git-codecommit.{region}.{git-domain}/v1/repos/{repository}
//	ssh://[{ssh-key-id}@]git-codecommit.{region}.{git-domain}/v1/repos/{repository}
//	https://{region}.{console-domain}/codesuite/codecommit/repositories/{repository}/pull-requests/{id}
//
// The first two are git-remote-codecommit URLs, where the profile is an AWS CLI
// profile name rather than a credential. The Git endpoints also match their
// FIPS variant (git-codecommit-fips.{region}) and scp-like SSH syntax. The Git
// domain is amazonaws.com in the standard and GovCloud partitions and
// amazonaws.com.cn in China. region and profile are empty when the URL does not
// name them.
func ParseRemote(remote string) (region, profile, repo string, ok bool) {
	return ResolveRemote(remote, "")
}

// ParseSupportedRemote extracts the region, profile, and repository from the
// supported profile-bearing opaque git-remote-codecommit origin form.
func ParseSupportedRemote(remote string) (region, profile, repo string, ok bool) {
	s := strings.TrimSpace(remote)
	if !strings.HasPrefix(strings.ToLower(s), "codecommit::") {
		return "", "", "", false
	}
	region, profile, repo, ok = ParseRemote(s)
	if !ok || region == "" || profile == "" {
		return "", "", "", false
	}
	return region, profile, repo, true
}

// ResolveRemote is ParseRemote for a remote whose SSH host alias resolved to
// resolvedHost (see scm.ResolveHost). The alias only stands in for the Git
// endpoint's host; the remote's path still names the repository.
func ResolveRemote(remote, resolvedHost string) (region, profile, repo string, ok bool) {
	s := strings.TrimSpace(remote)
	if address, found := helperAddress(s); found {
		return parseHelperRemote(address)
	}

	var host, path string
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", "", false
		}
		if region, repo, ok := parseConsoleURL(u); ok {
			return region, "", repo, true
		}
		if scheme := strings.ToLower(u.Scheme); scheme != "https" && scheme != "ssh" {
			return "", "", "", false
		}
		host = strings.ToLower(u.Hostname())
		path = u.EscapedPath()
	} else {
		// scp-like syntax: [user@]host:path. The first ':' separates host from
		// path; bail when a '/' precedes it (not scp form).
		c := strings.Index(s, ":")
		if c < 0 || strings.Contains(s[:c], "/") {
			return "", "", "", false
		}
		hostPart := s[:c]
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host = strings.ToLower(hostPart)
		path = s[c+1:]
	}

	region, ok = gitHostRegion(host)
	if !ok && resolvedHost != "" {
		region, ok = gitHostRegion(strings.ToLower(strings.TrimSpace(resolvedHost)))
	}
	if !ok {
		return "", "", "", false
	}
	segments := splitDecodePath(path)
	if len(segments) != 3 || segments[0] != "v1" || segments[1] != "repos" || !repositoryNamePattern.MatchString(segments[2]) {
		return "", "", "", false
	}
	return region, "", segments[2], true
}

// helperAddress returns what follows "codecommit:" in a git-remote-codecommit
// URL. Git hands a remote to the helper only for the "codecommit::" and
// "codecommit://" spellings; "codecommit:path" is scp-like syntax for an SSH
// host that happens to be named codecommit.
func helperAddress(s string) (string, bool) {
	if len(s) < len(helperScheme) || !strings.EqualFold(s[:len(helperScheme)], helperScheme) {
		return "", false
	}
	rest := s[len(helperScheme):]
	return rest, strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "//")
}

// parseHelperRemote parses what follows "codecommit:" in a git-remote-codecommit
// URL: "//[profile@]repository", which reads the region from the profile, or
// ":region://[profile@]repository". Like the helper itself, it splits the
// profile from the repository at the first '@'.
func parseHelperRemote(rest string) (region, profile, repo string, ok bool) {
	if after, found := strings.CutPrefix(rest, ":"); found {
		var address string
		region, address, found = strings.Cut(after, "://")
		if !found || !regionPattern.MatchString(region) {
			return "", "", "", false
		}
		rest = "//" + address
	}
	address, found := strings.CutPrefix(rest, "//")
	if !found {
		return "", "", "", false
	}
	if profile, repo, found = strings.Cut(address, "@"); !found {
		profile, repo = "", address
	} else if profile == "" || strings.ContainsAny(profile, " \t\r\n/?#") {
		return "", "", "", false
	}
	if !repositoryNamePattern.MatchString(repo) {
		return "", "", "", false
	}
	return region, profile, repo, true
}

// gitHostRegion returns the region a CodeCommit Git endpoint serves.
func gitHostRegion(host string) (string, bool) {
	rest, found := strings.CutPrefix(host, "git-codecommit.")
	if !found {
		rest, found = strings.CutPrefix(host, "git-codecommit-fips.")
	}
	if !found {
		return "", false
	}
	domain := "amazonaws.com"
	region, found := strings.CutSuffix(rest, "."+domain)
	if !found {
		domain = "amazonaws.com.cn"
		region, found = strings.CutSuffix(rest, "."+domain)
	}
	return region, found && regionPattern.MatchString(region) && gitDomain(region) == domain
}

// parseConsoleURL extracts the region and repository from a CodeCommit console
// URL under /codesuite/codecommit/repositories/{repository}. The region comes
// from the console's regional subdomain, or from its ?region= query when the
// URL uses the partition's global console host.
func parseConsoleURL(u *url.URL) (region, repo string, ok bool) {
	if !strings.EqualFold(u.Scheme, "https") {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	region = u.Query().Get("region")
	if subdomain, domain, found := strings.Cut(host, "."); found && regionPattern.MatchString(subdomain) {
		region, host = subdomain, domain
	}
	if !regionPattern.MatchString(region) || consoleDomain(region) != host {
		return "", "", false
	}
	segments := splitDecodePath(u.EscapedPath())
	if len(segments) < 4 || segments[0] != "codesuite" || segments[1] != "codecommit" || segments[2] != "repositories" || !repositoryNamePattern.MatchString(segments[3]) {
		return "", "", false
	}
	return region, segments[3], true
}

// consoleDomain returns the AWS Management Console domain of the partition that
// region belongs to. Both publish regional console endpoints
// ({region}.{domain}).
func consoleDomain(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "console.amazonaws.cn"
	}
	if strings.HasPrefix(region, "us-gov-") {
		return "console.amazonaws-us-gov.com"
	}
	return "console.aws.amazon.com"
}

func gitDomain(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "amazonaws.com.cn"
	}
	return "amazonaws.com"
}

func splitDecodePath(p string) []string {
	raw := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg == "" {
			continue
		}
		if dec, err := url.PathUnescape(seg); err == nil {
			out = append(out, dec)
		} else {
			out = append(out, seg)
		}
	}
	return out
}

// webPRURL builds the browsable console URL for a pull request. The region
// rides in the console's regional subdomain rather than a ?region= query so the
// URL still ends in the pull request ID, which scm.ExtractPRNumber reads back
// from a run's persisted PR URL.
func webPRURL(region, repo, id string) string {
	return "https://" + region + "." + consoleDomain(region) + "/codesuite/codecommit/repositories/" + url.PathEscape(repo) + "/pull-requests/" + id
}
