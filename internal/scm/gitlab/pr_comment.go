package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) AuthenticatedPRCommentPrincipal(ctx context.Context) (string, error) {
	out, err := h.cmd(ctx, "glab", h.commentAPIArgs("user")...).Output()
	if err != nil {
		return "", fmt.Errorf("glab api authenticated user: %w", err)
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(out, &user) != nil || user.ID <= 0 {
		return "", fmt.Errorf("GitLab authenticated user response was incomplete")
	}
	return strconv.FormatInt(user.ID, 10), nil
}

func (h *Host) ListPRComments(ctx context.Context, pr *scm.PR) ([]scm.PRComment, error) {
	project, iid, err := h.prCommentIdentity(pr)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%d/notes?per_page=100", project, iid)
	out, err := h.cmd(ctx, "glab", h.commentAPIArgs("--paginate", endpoint)...).Output()
	if err != nil {
		return nil, fmt.Errorf("glab api list merge request notes: %w", err)
	}
	var pages [][]gitlabNote
	if err := decodeNotePages(out, &pages); err != nil {
		return nil, fmt.Errorf("parse GitLab merge request notes: %w", err)
	}
	comments := make([]scm.PRComment, 0)
	for _, page := range pages {
		for _, raw := range page {
			// The notes endpoint mixes ordinary user-authored notes with
			// provider-generated system events (labels, title changes, etc.).
			// System notes cannot be the pipeline-owned comment and are not
			// writable through the ordinary-note update route, so ignore them
			// while keeping strict validation for every ordinary note.
			if raw.System {
				continue
			}
			comment, err := normalizeNote(raw, iid, 0)
			if err != nil {
				return nil, err
			}
			comments = append(comments, comment)
		}
	}
	return comments, nil
}

func (h *Host) CreatePRComment(ctx context.Context, pr *scm.PR, body string) (scm.PRComment, error) {
	project, iid, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%d/notes", project, iid)
	return h.writePRComment(ctx, endpoint, "POST", iid, 0, body)
}

func (h *Host) UpdatePRComment(ctx context.Context, pr *scm.PR, commentID, body string) (scm.PRComment, error) {
	project, iid, err := h.prCommentIdentity(pr)
	if err != nil {
		return scm.PRComment{}, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(commentID), 10, 64)
	if err != nil || id <= 0 {
		return scm.PRComment{}, fmt.Errorf("invalid GitLab merge request note id %q", commentID)
	}
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%d/notes/%d", project, iid, id)
	return h.writePRComment(ctx, endpoint, "PUT", iid, id, body)
}

func (h *Host) writePRComment(ctx context.Context, endpoint, method string, iid int, id int64, body string) (scm.PRComment, error) {
	payload, _ := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	args := h.commentAPIArgs("--method", method, endpoint, "--input", "-")
	cmd := h.cmd(ctx, "glab", args...)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		return scm.PRComment{}, fmt.Errorf("glab api %s merge request note: %w", strings.ToLower(method), err)
	}
	var raw gitlabNote
	if err := json.Unmarshal(out, &raw); err != nil {
		return scm.PRComment{}, fmt.Errorf("parse GitLab merge request note write: %w", err)
	}
	comment, err := normalizeNote(raw, iid, id)
	if err != nil {
		return scm.PRComment{}, err
	}
	if comment.Body != body {
		return scm.PRComment{}, fmt.Errorf("GitLab merge request note write did not preserve the proposed body")
	}
	return comment, nil
}

type gitlabNote struct {
	ID          int64   `json:"id"`
	Body        *string `json:"body"`
	NoteableIID int     `json:"noteable_iid"`
	System      bool    `json:"system"`
	WebURL      string  `json:"web_url"`
	Author      struct {
		ID int64 `json:"id"`
	} `json:"author"`
}

func normalizeNote(raw gitlabNote, iid int, expectedID int64) (scm.PRComment, error) {
	if raw.ID <= 0 || raw.Body == nil || raw.NoteableIID != iid || raw.System {
		return scm.PRComment{}, fmt.Errorf("GitLab merge request note response was incomplete or belonged to a different review object")
	}
	if expectedID > 0 && raw.ID != expectedID {
		return scm.PRComment{}, fmt.Errorf("GitLab merge request note identity mismatch: got %d, expected %d", raw.ID, expectedID)
	}
	principal := ""
	if raw.Author.ID > 0 {
		principal = strconv.FormatInt(raw.Author.ID, 10)
	}
	return scm.PRComment{ID: strconv.FormatInt(raw.ID, 10), Body: *raw.Body, URL: raw.WebURL, Principal: principal}, nil
}

func (h *Host) prCommentIdentity(pr *scm.PR) (string, int, error) {
	path := strings.Trim(strings.TrimSpace(h.projectPath), "/")
	if path == "" {
		return "", 0, fmt.Errorf("cannot determine GitLab project for merge request comments")
	}
	iid := 0
	if pr != nil && strings.TrimSpace(pr.Number) != "" {
		iid, _ = strconv.Atoi(strings.TrimSpace(pr.Number))
	}
	if pr != nil && strings.TrimSpace(pr.URL) != "" {
		fromURL, err := parseMergeRequestURL(pr.URL, h.host, path)
		if err != nil {
			return "", 0, fmt.Errorf("validate GitLab merge request comment target: %w", err)
		}
		if iid != 0 && iid != fromURL {
			return "", 0, fmt.Errorf("GitLab merge request number and URL disagree")
		}
		iid = fromURL
	}
	if iid <= 0 {
		return "", 0, fmt.Errorf("GitLab merge request comment target requires a positive number")
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "%2F"), iid, nil
}

func (h *Host) commentAPIArgs(args ...string) []string {
	out := []string{"api"}
	if h.host != "" {
		out = append(out, "--hostname", h.host)
	}
	return append(out, args...)
}

func decodeNotePages(out []byte, pages *[][]gitlabNote) error {
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var page []gitlabNote
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
