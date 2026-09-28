package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Ordinary PR comments are issue comments in GitHub's API. Keep this separate
// from GetReviewComments, which reads review threads and deliberately filters
// them to registered review bots.
func (h *Host) ListPRComments(ctx context.Context, pr *scm.PR) ([]scm.PRComment, error) {
	repo, number, err := h.prCommentIdentity(pr)
	if err != nil {
		return nil, err
	}
	args := h.apiArgs("--paginate", "--method", "GET", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, number))
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api list PR comments: %w", err)
	}
	var pages [][]githubIssueComment
	if err := decodeJSONArrayPages(out, &pages); err != nil {
		return nil, fmt.Errorf("parse gh PR comments: %w", err)
	}
	comments := make([]scm.PRComment, 0)
	for _, page := range pages {
		for _, raw := range page {
			comment, err := h.normalizeIssueComment(raw, repo, number, 0)
			if err != nil {
				return nil, err
			}
			comments = append(comments, comment)
		}
	}
	return comments, nil
}

func (h *Host) CreatePRComment(ctx context.Context, pr *scm.PR, body string) (scm.PRComment, error) {
	repo, number, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	return h.writePRComment(ctx, repo, number, 0, body)
}

func (h *Host) UpdatePRComment(ctx context.Context, pr *scm.PR, commentID, body string) (scm.PRComment, error) {
	repo, number, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(commentID), 10, 64)
	if err != nil || id <= 0 {
		return scm.PRComment{}, fmt.Errorf("invalid GitHub PR comment id %q", commentID)
	}
	return h.writePRComment(ctx, repo, number, id, body)
}

func (h *Host) writePRComment(ctx context.Context, repo string, number int, id int64, body string) (scm.PRComment, error) {
	payload, _ := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	method := "POST"
	endpoint := fmt.Sprintf("repos/%s/issues/%d/comments", repo, number)
	if id > 0 {
		method = "PATCH"
		endpoint = fmt.Sprintf("repos/%s/issues/comments/%d", repo, id)
	}
	args := h.apiArgs("--method", method, endpoint, "--input", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		return scm.PRComment{}, fmt.Errorf("gh api %s PR comment: %w", strings.ToLower(method), err)
	}
	var raw githubIssueComment
	if err := json.Unmarshal(out, &raw); err != nil {
		return scm.PRComment{}, fmt.Errorf("parse gh PR comment write: %w", err)
	}
	comment, err := h.normalizeIssueComment(raw, repo, number, id)
	if err != nil {
		return scm.PRComment{}, err
	}
	if comment.Body != body {
		return scm.PRComment{}, fmt.Errorf("GitHub PR comment write did not preserve the proposed body")
	}
	return comment, nil
}

type githubIssueComment struct {
	ID       int64   `json:"id"`
	Body     *string `json:"body"`
	HTMLURL  string  `json:"html_url"`
	IssueURL string  `json:"issue_url"`
}

func (h *Host) normalizeIssueComment(raw githubIssueComment, repo string, number int, expectedID int64) (scm.PRComment, error) {
	if raw.ID <= 0 || raw.Body == nil {
		return scm.PRComment{}, fmt.Errorf("GitHub PR comment response was incomplete")
	}
	if expectedID > 0 && raw.ID != expectedID {
		return scm.PRComment{}, fmt.Errorf("GitHub PR comment identity mismatch: got %d, expected %d", raw.ID, expectedID)
	}
	suffix := fmt.Sprintf("/repos/%s/issues/%d", repo, number)
	if !strings.HasSuffix(strings.TrimRight(raw.IssueURL, "/"), suffix) {
		return scm.PRComment{}, fmt.Errorf("GitHub PR comment belongs to a different review object")
	}
	return scm.PRComment{ID: strconv.FormatInt(raw.ID, 10), Body: *raw.Body, URL: raw.HTMLURL}, nil
}

func (h *Host) prCommentIdentity(pr *scm.PR) (string, int, error) {
	repo := strings.Trim(strings.TrimSpace(h.repoSlug()), "/")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", 0, fmt.Errorf("cannot determine GitHub repository for PR comments")
	}
	number := 0
	if pr != nil && strings.TrimSpace(pr.Number) != "" {
		number, _ = strconv.Atoi(strings.TrimSpace(pr.Number))
	}
	if pr != nil && strings.TrimSpace(pr.URL) != "" {
		fromURL, err := parsePullRequestURL(pr.URL, h.host, repo)
		if err != nil {
			return "", 0, fmt.Errorf("validate GitHub PR comment target: %w", err)
		}
		if number != 0 && number != fromURL {
			return "", 0, fmt.Errorf("GitHub PR number and URL disagree")
		}
		number = fromURL
	}
	if number <= 0 {
		return "", 0, fmt.Errorf("GitHub PR comment target requires a positive PR number")
	}
	return repo, number, nil
}

func (h *Host) apiArgs(args ...string) []string {
	out := []string{"api"}
	if h.host != "" {
		out = append(out, "--hostname", h.host)
	}
	return append(out, args...)
}

func decodeJSONArrayPages[T any](out []byte, pages *[][]T) error {
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var page []T
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if page == nil {
			return fmt.Errorf("expected JSON array")
		}
		*pages = append(*pages, page)
	}
	if len(*pages) == 0 {
		return fmt.Errorf("expected at least one JSON array")
	}
	return nil
}

var _ scm.ManagedPRCommentHost = (*Host)(nil)
