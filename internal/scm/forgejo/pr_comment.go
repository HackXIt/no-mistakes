package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const forgejoCommentPageSize = 50
const maxForgejoCommentPages = 100

// forgejo-axi's raw API contract is used here for ordinary issue comments:
// `api METHOD PATH --data JSON --json` emits {status,data}. Pull requests share
// the issue-comment endpoint in Forgejo. Pagination is explicit so an owned
// marker beyond the first page can never be mistaken for absence.
func (h *Host) ListPRComments(ctx context.Context, pr *scm.PR) ([]scm.PRComment, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return nil, err
	}
	comments := make([]scm.PRComment, 0)
	for page := 1; page <= maxForgejoCommentPages; page++ {
		var response struct {
			Status int                    `json:"status"`
			Data   *[]forgejoIssueComment `json:"data"`
		}
		endpoint := fmt.Sprintf("repos/%s/issues/%s/comments?limit=%d&page=%d", h.repository, number, forgejoCommentPageSize, page)
		if err := h.runJSON(ctx, "api", []string{"GET", endpoint}, &response); err != nil {
			return nil, err
		}
		if response.Status != 200 || response.Data == nil {
			return nil, fmt.Errorf("forgejo-axi api: incomplete pull comment page")
		}
		for _, raw := range *response.Data {
			comment, err := h.normalizeIssueComment(raw, number, 0)
			if err != nil {
				return nil, err
			}
			comments = append(comments, comment)
		}
		if len(*response.Data) < forgejoCommentPageSize {
			return comments, nil
		}
	}
	return nil, fmt.Errorf("forgejo-axi pull comment pagination exceeded %d pages", maxForgejoCommentPages)
}

func (h *Host) CreatePRComment(ctx context.Context, pr *scm.PR, body string) (scm.PRComment, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	endpoint := fmt.Sprintf("repos/%s/issues/%s/comments", h.repository, number)
	return h.writePRComment(ctx, endpoint, "POST", number, 0, body)
}

func (h *Host) UpdatePRComment(ctx context.Context, pr *scm.PR, commentID, body string) (scm.PRComment, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(commentID), 10, 64)
	if err != nil || id <= 0 {
		return scm.PRComment{}, fmt.Errorf("invalid Forgejo pull comment id %q", commentID)
	}
	endpoint := fmt.Sprintf("repos/%s/issues/comments/%d", h.repository, id)
	return h.writePRComment(ctx, endpoint, "PATCH", number, id, body)
}

func (h *Host) writePRComment(ctx context.Context, endpoint, method, number string, id int64, body string) (scm.PRComment, error) {
	payload, _ := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	var response struct {
		Status int                  `json:"status"`
		Data   *forgejoIssueComment `json:"data"`
	}
	if err := h.runJSON(ctx, "api", []string{method, endpoint, "--data", string(payload)}, &response); err != nil {
		return scm.PRComment{}, err
	}
	wantStatus := 201
	if method == "PATCH" {
		wantStatus = 200
	}
	if response.Status != wantStatus || response.Data == nil {
		return scm.PRComment{}, fmt.Errorf("forgejo-axi api: incomplete pull comment write")
	}
	comment, err := h.normalizeIssueComment(*response.Data, number, id)
	if err != nil {
		return scm.PRComment{}, err
	}
	if comment.Body != body {
		return scm.PRComment{}, fmt.Errorf("Forgejo pull comment write did not preserve the proposed body")
	}
	return comment, nil
}

type forgejoIssueComment struct {
	ID       int64   `json:"id"`
	Body     *string `json:"body"`
	HTMLURL  string  `json:"html_url"`
	IssueURL string  `json:"issue_url"`
}

func (h *Host) normalizeIssueComment(raw forgejoIssueComment, number string, expectedID int64) (scm.PRComment, error) {
	if raw.ID <= 0 || raw.Body == nil {
		return scm.PRComment{}, fmt.Errorf("Forgejo pull comment response was incomplete")
	}
	if expectedID > 0 && raw.ID != expectedID {
		return scm.PRComment{}, fmt.Errorf("Forgejo pull comment identity mismatch: got %d, expected %d", raw.ID, expectedID)
	}
	suffix := fmt.Sprintf("/api/v1/repos/%s/issues/%s", h.repository, number)
	if !strings.HasSuffix(strings.TrimRight(raw.IssueURL, "/"), suffix) {
		return scm.PRComment{}, fmt.Errorf("Forgejo pull comment belongs to a different review object")
	}
	return scm.PRComment{ID: strconv.FormatInt(raw.ID, 10), Body: *raw.Body, URL: raw.HTMLURL}, nil
}

var _ scm.ManagedPRCommentHost = (*Host)(nil)
