package azuredevops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Azure DevOps models PR comments as comments inside threads. The managed
// validation comment is always a root comment; its durable identity is encoded
// as "threadID:commentID" so updates cannot bind to a sibling thread.
func (h *Host) ListPRComments(ctx context.Context, pr *scm.PR) ([]scm.PRComment, error) {
	id, err := h.prCommentIdentity(pr)
	if err != nil {
		return nil, err
	}
	out, err := outputJSON(h.cmd(ctx, "az", h.threadInvokeArgs("pullRequestThreads", id, nil, "GET", "")...))
	if err != nil {
		return nil, fmt.Errorf("az devops invoke list PR comment threads: %w", err)
	}
	var response struct {
		Count int            `json:"count"`
		Value *[]azureThread `json:"value"`
	}
	if err := json.Unmarshal(out, &response); err != nil || response.Value == nil || response.Count != len(*response.Value) {
		return nil, fmt.Errorf("parse Azure DevOps PR comment threads: incomplete response")
	}
	comments := make([]scm.PRComment, 0)
	for _, thread := range *response.Value {
		if thread.ID <= 0 || thread.Comments == nil {
			return nil, fmt.Errorf("Azure DevOps PR comment thread was incomplete")
		}
		for _, raw := range *thread.Comments {
			if raw.ParentCommentID != 0 || raw.IsDeleted {
				continue
			}
			comment, err := normalizeAzureComment(thread.ID, raw, 0, 0)
			if err != nil {
				return nil, err
			}
			comments = append(comments, comment)
		}
	}
	return comments, nil
}

func (h *Host) CreatePRComment(ctx context.Context, pr *scm.PR, body string) (scm.PRComment, error) {
	id, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	payload := struct {
		Comments []struct {
			ParentCommentID int    `json:"parentCommentId"`
			Content         string `json:"content"`
			CommentType     int    `json:"commentType"`
		} `json:"comments"`
		Status int `json:"status"`
	}{Status: 1}
	payload.Comments = append(payload.Comments, struct {
		ParentCommentID int    `json:"parentCommentId"`
		Content         string `json:"content"`
		CommentType     int    `json:"commentType"`
	}{Content: body, CommentType: 1})
	path, err := writeAzureCommentPayload(payload)
	if err != nil {
		return scm.PRComment{}, err
	}
	defer os.Remove(path)
	out, err := outputJSON(h.cmd(ctx, "az", h.threadInvokeArgs("pullRequestThreads", id, nil, "POST", path)...))
	if err != nil {
		return scm.PRComment{}, fmt.Errorf("az devops invoke create PR comment thread: %w", err)
	}
	var thread azureThread
	if err := json.Unmarshal(out, &thread); err != nil || thread.ID <= 0 || thread.Comments == nil || len(*thread.Comments) != 1 {
		return scm.PRComment{}, fmt.Errorf("parse Azure DevOps PR comment create: incomplete response")
	}
	comment, err := normalizeAzureComment(thread.ID, (*thread.Comments)[0], 0, 0)
	if err != nil {
		return scm.PRComment{}, err
	}
	if comment.Body != body {
		return scm.PRComment{}, fmt.Errorf("Azure DevOps PR comment create did not preserve the proposed body")
	}
	return comment, nil
}

func (h *Host) UpdatePRComment(ctx context.Context, pr *scm.PR, commentID, body string) (scm.PRComment, error) {
	id, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	threadID, rootID, err := parseAzureCommentID(commentID)
	if err != nil {
		return scm.PRComment{}, err
	}
	path, err := writeAzureCommentPayload(struct {
		Content string `json:"content"`
	}{Content: body})
	if err != nil {
		return scm.PRComment{}, err
	}
	defer os.Remove(path)
	routes := []string{"threadId=" + strconv.Itoa(threadID), "commentId=" + strconv.Itoa(rootID)}
	out, err := outputJSON(h.cmd(ctx, "az", h.threadInvokeArgs("pullRequestThreadComments", id, routes, "PATCH", path)...))
	if err != nil {
		return scm.PRComment{}, fmt.Errorf("az devops invoke update PR comment: %w", err)
	}
	var raw azureComment
	if err := json.Unmarshal(out, &raw); err != nil {
		return scm.PRComment{}, fmt.Errorf("parse Azure DevOps PR comment update: %w", err)
	}
	comment, err := normalizeAzureComment(threadID, raw, threadID, rootID)
	if err != nil {
		return scm.PRComment{}, err
	}
	if comment.Body != body {
		return scm.PRComment{}, fmt.Errorf("Azure DevOps PR comment update did not preserve the proposed body")
	}
	return comment, nil
}

type azureThread struct {
	ID       int             `json:"id"`
	Comments *[]azureComment `json:"comments"`
}

type azureComment struct {
	ID              int     `json:"id"`
	ParentCommentID int     `json:"parentCommentId"`
	Content         *string `json:"content"`
	CommentType     int     `json:"commentType"`
	IsDeleted       bool    `json:"isDeleted"`
}

func normalizeAzureComment(threadID int, raw azureComment, expectedThread, expectedComment int) (scm.PRComment, error) {
	if threadID <= 0 || raw.ID <= 0 || raw.ParentCommentID != 0 || raw.Content == nil || raw.CommentType != 1 || raw.IsDeleted {
		return scm.PRComment{}, fmt.Errorf("Azure DevOps PR comment response was incomplete")
	}
	if expectedThread > 0 && (threadID != expectedThread || raw.ID != expectedComment) {
		return scm.PRComment{}, fmt.Errorf("Azure DevOps PR comment identity mismatch")
	}
	return scm.PRComment{ID: fmt.Sprintf("%d:%d", threadID, raw.ID), Body: *raw.Content}, nil
}

func parseAzureCommentID(raw string) (int, int, error) {
	left, right, ok := strings.Cut(strings.TrimSpace(raw), ":")
	threadID, err1 := strconv.Atoi(left)
	commentID, err2 := strconv.Atoi(right)
	if !ok || err1 != nil || err2 != nil || threadID <= 0 || commentID <= 0 {
		return 0, 0, fmt.Errorf("invalid Azure DevOps PR comment id %q", raw)
	}
	return threadID, commentID, nil
}

func (h *Host) threadInvokeArgs(resource string, prID int, extraRoutes []string, method, input string) []string {
	routes := []string{"project=" + h.project, "repositoryId=" + h.repo, "pullRequestId=" + strconv.Itoa(prID)}
	routes = append(routes, extraRoutes...)
	args := []string{"devops", "invoke", "--area", "git", "--resource", resource, "--route-parameters"}
	args = append(args, routes...)
	args = append(args, "--api-version", "7.1", "--http-method", method)
	if input != "" {
		args = append(args, "--in-file", input)
	}
	args = append(args, h.orgArgs()...)
	return append(args, "--output", "json")
}

func writeAzureCommentPayload(value any) (string, error) {
	file, err := os.CreateTemp("", "no-mistakes-azure-comment-*.json")
	if err != nil {
		return "", fmt.Errorf("create Azure DevOps comment payload: %w", err)
	}
	path := file.Name()
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(value); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("encode Azure DevOps comment payload: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close Azure DevOps comment payload: %w", err)
	}
	return path, nil
}

func (h *Host) prCommentIdentity(pr *scm.PR) (int, error) {
	id, err := strconv.Atoi(h.prID(pr))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("Azure DevOps PR comment target requires a positive PR id")
	}
	if pr != nil && strings.TrimSpace(pr.URL) != "" {
		org, project, repo, ok := ParseRemote(pr.URL)
		fromURL, numberErr := scm.ExtractPRNumber(pr.URL)
		if !ok || !strings.EqualFold(org, h.org) || !strings.EqualFold(project, h.project) || !strings.EqualFold(repo, h.repo) || numberErr != nil || fromURL != strconv.Itoa(id) {
			return 0, fmt.Errorf("Azure DevOps PR URL does not match the configured review object")
		}
	}
	return id, nil
}

var _ scm.ManagedPRCommentHost = (*Host)(nil)
