package safeurl

import (
	"net/url"
	"regexp"
	"strings"
)

var httpURLPattern = regexp.MustCompile(`https?://[^\s'"<>]+`)

// RedactedUserinfo is the placeholder Redact substitutes for URL userinfo.
const RedactedUserinfo = "redacted"

// Redact hides URL userinfo while leaving non-URL and credential-free values
// unchanged.
func Redact(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = url.User(RedactedUserinfo)
	return parsed.String()
}

func RedactText(text string) string {
	return httpURLPattern.ReplaceAllStringFunc(text, Redact)
}
