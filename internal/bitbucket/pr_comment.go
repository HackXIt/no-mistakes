package bitbucket

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) ListPRComments(ctx context.Context, pr *scm.PR) ([]scm.PRComment, error) {
	id, err := h.prCommentIdentity(pr)
	if err != nil {
		return nil, err
	}
	raw, err := h.client.ListPRComments(ctx, h.repo, id)
	if err != nil {
		return nil, err
	}
	comments := make([]scm.PRComment, 0, len(raw))
	for _, comment := range raw {
		comments = append(comments, scm.PRComment{ID: strconv.Itoa(comment.ID), Body: comment.Body, URL: comment.URL})
	}
	return comments, nil
}

func (h *Host) CreatePRComment(ctx context.Context, pr *scm.PR, body string) (scm.PRComment, error) {
	id, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	comment, err := h.client.CreatePRComment(ctx, h.repo, id, body)
	if err != nil {
		return scm.PRComment{}, err
	}
	return scm.PRComment{ID: strconv.Itoa(comment.ID), Body: comment.Body, URL: comment.URL}, nil
}

func (h *Host) UpdatePRComment(ctx context.Context, pr *scm.PR, commentID, body string) (scm.PRComment, error) {
	id, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	commentNumber, err := strconv.Atoi(strings.TrimSpace(commentID))
	if err != nil || commentNumber <= 0 {
		return scm.PRComment{}, fmt.Errorf("invalid Bitbucket PR comment id %q", commentID)
	}
	comment, err := h.client.UpdatePRComment(ctx, h.repo, id, commentNumber, body)
	if err != nil {
		return scm.PRComment{}, err
	}
	return scm.PRComment{ID: strconv.Itoa(comment.ID), Body: comment.Body, URL: comment.URL}, nil
}

func (h *Host) prCommentIdentity(pr *scm.PR) (int, error) {
	if pr == nil {
		return 0, fmt.Errorf("missing Bitbucket pull identity")
	}
	id, err := strconv.Atoi(strings.TrimSpace(pr.Number))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid Bitbucket pull number %q", pr.Number)
	}
	if strings.TrimSpace(pr.URL) != "" {
		repo, err := ParseRepoRef(pr.URL)
		if err != nil || !strings.EqualFold(repo.Workspace, h.repo.Workspace) || !strings.EqualFold(repo.RepoSlug, h.repo.RepoSlug) {
			return 0, fmt.Errorf("Bitbucket pull URL does not match the configured repository")
		}
		fromURL, err := scm.ExtractPRNumber(pr.URL)
		if err != nil || fromURL != strconv.Itoa(id) {
			return 0, fmt.Errorf("Bitbucket pull URL does not match the configured pull number")
		}
	}
	return id, nil
}

var _ scm.ManagedPRCommentHost = (*Host)(nil)
