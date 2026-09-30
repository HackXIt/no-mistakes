package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const giteaCommentPageSize = 50
const maxGiteaCommentPages = 100

func (h *Host) AuthenticatedPRCommentPrincipal(ctx context.Context) (string, error) {
	out, err := h.cmd(ctx, "tea", "api", "--login", h.login, "/user").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tea api authenticated user: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(bytesTrimToJSON(out), &user) != nil || user.ID <= 0 {
		return "", fmt.Errorf("Gitea authenticated user response was incomplete")
	}
	return strconv.FormatInt(user.ID, 10), nil
}

func (h *Host) ListPRComments(ctx context.Context, pr *scm.PR) ([]scm.PRComment, error) {
	owner, repo, number, err := h.prCommentIdentity(pr)
	if err != nil {
		return nil, err
	}
	comments := make([]scm.PRComment, 0)
	for page := 1; page <= maxGiteaCommentPages; page++ {
		endpoint := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?limit=%d&page=%d", owner, repo, number, giteaCommentPageSize, page)
		out, err := h.cmd(ctx, "tea", "api", "--login", h.login, endpoint).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("tea api list pull comments: %s: %w", strings.TrimSpace(string(out)), err)
		}
		var raw []giteaIssueComment
		if err := json.Unmarshal(bytesTrimToJSON(out), &raw); err != nil || raw == nil {
			return nil, fmt.Errorf("parse tea pull comments page %d: expected JSON array", page)
		}
		for _, item := range raw {
			comment, err := normalizeGiteaComment(item, owner, repo, number, 0)
			if err != nil {
				return nil, err
			}
			comments = append(comments, comment)
		}
		if len(raw) == 0 {
			return comments, nil
		}
	}
	return nil, fmt.Errorf("tea pull comment pagination exceeded %d pages", maxGiteaCommentPages)
}

func (h *Host) CreatePRComment(ctx context.Context, pr *scm.PR, body string) (scm.PRComment, error) {
	owner, repo, number, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	return h.writePRComment(ctx, endpoint, "POST", owner, repo, number, 0, body)
}

func (h *Host) UpdatePRComment(ctx context.Context, pr *scm.PR, commentID, body string) (scm.PRComment, error) {
	owner, repo, number, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(commentID), 10, 64)
	if err != nil || id <= 0 {
		return scm.PRComment{}, fmt.Errorf("invalid Gitea pull comment id %q", commentID)
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, id)
	return h.writePRComment(ctx, endpoint, "PATCH", owner, repo, number, id, body)
}

func (h *Host) writePRComment(ctx context.Context, endpoint, method, owner, repo string, number int, id int64, body string) (scm.PRComment, error) {
	args := []string{"api", "--login", h.login, "--method", method, "--field", "body=" + body, endpoint}
	if err := scm.CheckWindowsCommandLine(h.goos, "tea", args); err != nil {
		return scm.PRComment{}, fmt.Errorf("Gitea managed comment cannot be published through tea on Windows; reduce validation detail (for example, set pr.appendix: minimal) or retry from a non-Windows daemon: %w", err)
	}
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return scm.PRComment{}, fmt.Errorf("tea api %s pull comment: %s: %w", strings.ToLower(method), strings.TrimSpace(string(out)), err)
	}
	var raw giteaIssueComment
	if err := json.Unmarshal(bytesTrimToJSON(out), &raw); err != nil {
		return scm.PRComment{}, fmt.Errorf("parse tea pull comment write: %w", err)
	}
	comment, err := normalizeGiteaComment(raw, owner, repo, number, id)
	if err != nil {
		return scm.PRComment{}, err
	}
	if comment.Body != body {
		return scm.PRComment{}, fmt.Errorf("Gitea pull comment write did not preserve the proposed body")
	}
	return comment, nil
}

type giteaIssueComment struct {
	ID       int64   `json:"id"`
	Body     *string `json:"body"`
	HTMLURL  string  `json:"html_url"`
	IssueURL string  `json:"issue_url"`
	User     struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

func normalizeGiteaComment(raw giteaIssueComment, owner, repo string, number int, expectedID int64) (scm.PRComment, error) {
	if raw.ID <= 0 || raw.Body == nil {
		return scm.PRComment{}, fmt.Errorf("Gitea pull comment response was incomplete")
	}
	if expectedID > 0 && raw.ID != expectedID {
		return scm.PRComment{}, fmt.Errorf("Gitea pull comment identity mismatch: got %d, expected %d", raw.ID, expectedID)
	}
	suffix := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	if !strings.HasSuffix(strings.TrimRight(raw.IssueURL, "/"), suffix) {
		return scm.PRComment{}, fmt.Errorf("Gitea pull comment belongs to a different review object")
	}
	principal := ""
	if raw.User.ID > 0 {
		principal = strconv.FormatInt(raw.User.ID, 10)
	}
	return scm.PRComment{ID: strconv.FormatInt(raw.ID, 10), Body: *raw.Body, URL: raw.HTMLURL, Principal: principal}, nil
}

func (h *Host) prCommentIdentity(pr *scm.PR) (string, string, int, error) {
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok {
		return "", "", 0, fmt.Errorf("invalid Gitea repository")
	}
	id, err := giteaPRNumber(pr)
	if err != nil {
		return "", "", 0, err
	}
	number, err := strconv.Atoi(id)
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("invalid Gitea pull number")
	}
	if pr != nil && strings.TrimSpace(pr.URL) != "" {
		parsed, err := url.Parse(strings.TrimSpace(pr.URL))
		if err != nil || parsed.Host == "" || (h.host != "" && !strings.EqualFold(parsed.Hostname(), h.host)) {
			return "", "", 0, fmt.Errorf("Gitea pull URL does not match the configured host")
		}
		wantPath := fmt.Sprintf("/%s/%s/pulls/%d", owner, repo, number)
		if strings.TrimRight(parsed.Path, "/") != wantPath {
			return "", "", 0, fmt.Errorf("Gitea pull URL does not match the configured repository and number")
		}
	}
	return owner, repo, number, nil
}

var _ scm.ManagedPRCommentHost = (*Host)(nil)
