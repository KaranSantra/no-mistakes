package codecommit

import (
	"context"
	"errors"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetPRContent reads the pull request's raw title and description, preserving
// whitespace and Unicode. The description key is absent when a pull request
// has none, so absence reads as an empty body - but only once showPR has
// proven the read complete (matching ID, a single target in this repository),
// the title CodeCommit requires is present, and no description is null or
// mistyped (see description).
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return scm.PRContent{}, err
	}
	if strings.TrimSpace(got.Title) == "" {
		return scm.PRContent{}, errors.New("aws codecommit get-pull-request: incomplete raw content")
	}
	return scm.PRContent{Title: got.Title, Body: string(got.Description)}, nil
}

var _ scm.PRContentReader = (*Host)(nil)
