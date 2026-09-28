package db

import (
	"database/sql"
	"fmt"
	"strings"
)

type PendingManagedPRComment struct {
	RepoID        string
	Provider      string
	PRNumber      string
	PRURL         string
	HeadSHA       string
	Principal     string
	MarkerDigest  string
	PayloadDigest string
	Body          string
	CreatedAt     int64
}

type ManagedPRCommentBinding struct {
	RepoID    string
	Provider  string
	PRNumber  string
	PRURL     string
	CommentID string
	CreatedAt int64
}

func (d *DB) GetManagedPRCommentBinding(repoID, provider, prNumber string) (*ManagedPRCommentBinding, error) {
	repoID = strings.TrimSpace(repoID)
	provider = strings.TrimSpace(provider)
	prNumber = strings.TrimSpace(prNumber)
	if repoID == "" || provider == "" || prNumber == "" {
		return nil, fmt.Errorf("get managed PR comment binding: repo, provider and PR number are required")
	}
	var binding ManagedPRCommentBinding
	err := d.sql.QueryRow(
		`SELECT repo_id, provider, pr_number, pr_url, comment_id, created_at
		   FROM managed_pr_comments
		  WHERE repo_id = ? AND provider = ? AND pr_number = ?`,
		repoID, provider, prNumber,
	).Scan(&binding.RepoID, &binding.Provider, &binding.PRNumber, &binding.PRURL, &binding.CommentID, &binding.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get managed PR comment binding: %w", err)
	}
	return &binding, nil
}

func (d *DB) GetPendingManagedPRComment(repoID, provider, prNumber string) (*PendingManagedPRComment, error) {
	repoID = strings.TrimSpace(repoID)
	provider = strings.TrimSpace(provider)
	prNumber = strings.TrimSpace(prNumber)
	if repoID == "" || provider == "" || prNumber == "" {
		return nil, fmt.Errorf("get pending managed PR comment: repo, provider and PR number are required")
	}
	var pending PendingManagedPRComment
	err := d.sql.QueryRow(
		`SELECT repo_id, provider, pr_number, pr_url, head_sha, principal, marker_digest, payload_digest, body, created_at
		   FROM pending_managed_pr_comments
		  WHERE repo_id = ? AND provider = ? AND pr_number = ?`,
		repoID, provider, prNumber,
	).Scan(&pending.RepoID, &pending.Provider, &pending.PRNumber, &pending.PRURL, &pending.HeadSHA, &pending.Principal, &pending.MarkerDigest, &pending.PayloadDigest, &pending.Body, &pending.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending managed PR comment: %w", err)
	}
	return &pending, nil
}

func (d *DB) BeginManagedPRCommentCreate(pending PendingManagedPRComment) error {
	if err := validatePendingManagedPRComment(pending); err != nil {
		return err
	}
	binding, err := d.GetManagedPRCommentBinding(pending.RepoID, pending.Provider, pending.PRNumber)
	if err != nil {
		return err
	}
	if binding != nil {
		return fmt.Errorf("begin managed PR comment create: review object is already bound to comment %s", binding.CommentID)
	}
	_, err = d.sql.Exec(
		`INSERT INTO pending_managed_pr_comments
		    (repo_id, provider, pr_number, pr_url, head_sha, principal, marker_digest, payload_digest, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, provider, pr_number) DO NOTHING`,
		pending.RepoID, pending.Provider, pending.PRNumber, pending.PRURL, pending.HeadSHA,
		pending.Principal, pending.MarkerDigest, pending.PayloadDigest, pending.Body, now(),
	)
	if err != nil {
		return fmt.Errorf("begin managed PR comment create: %w", err)
	}
	existing, err := d.GetPendingManagedPRComment(pending.RepoID, pending.Provider, pending.PRNumber)
	if err != nil {
		return err
	}
	if existing == nil || !samePendingManagedPRComment(*existing, pending) {
		return fmt.Errorf("begin managed PR comment create: a different create intent is already pending")
	}
	return nil
}

func (d *DB) ReplacePendingManagedPRComment(existing, replacement PendingManagedPRComment) error {
	if err := validatePendingManagedPRComment(existing); err != nil {
		return err
	}
	if err := validatePendingManagedPRComment(replacement); err != nil {
		return err
	}
	if existing.RepoID != replacement.RepoID || existing.Provider != replacement.Provider || existing.PRNumber != replacement.PRNumber || existing.PRURL != replacement.PRURL || existing.Principal != replacement.Principal {
		return fmt.Errorf("replace pending managed PR comment: ownership identity changed")
	}
	result, err := d.sql.Exec(
		`UPDATE pending_managed_pr_comments
		    SET head_sha = ?, marker_digest = ?, payload_digest = ?, body = ?, created_at = ?
		  WHERE repo_id = ? AND provider = ? AND pr_number = ? AND pr_url = ?
		    AND head_sha = ? AND principal = ? AND marker_digest = ? AND payload_digest = ? AND body = ?`,
		replacement.HeadSHA, replacement.MarkerDigest, replacement.PayloadDigest, replacement.Body, now(),
		existing.RepoID, existing.Provider, existing.PRNumber, existing.PRURL, existing.HeadSHA,
		existing.Principal, existing.MarkerDigest, existing.PayloadDigest, existing.Body,
	)
	if err != nil {
		return fmt.Errorf("replace pending managed PR comment: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fmt.Errorf("replace pending managed PR comment: pending intent changed")
	}
	return nil
}

func validatePendingManagedPRComment(p PendingManagedPRComment) error {
	if strings.TrimSpace(p.RepoID) == "" || strings.TrimSpace(p.Provider) == "" || strings.TrimSpace(p.PRNumber) == "" || strings.TrimSpace(p.PRURL) == "" || strings.TrimSpace(p.HeadSHA) == "" || strings.TrimSpace(p.Principal) == "" || strings.TrimSpace(p.MarkerDigest) == "" || strings.TrimSpace(p.PayloadDigest) == "" || p.Body == "" {
		return fmt.Errorf("managed PR comment create intent is incomplete")
	}
	return nil
}

func samePendingManagedPRComment(a, b PendingManagedPRComment) bool {
	return a.RepoID == b.RepoID && a.Provider == b.Provider && a.PRNumber == b.PRNumber && a.PRURL == b.PRURL && a.HeadSHA == b.HeadSHA && a.Principal == b.Principal && a.MarkerDigest == b.MarkerDigest && a.PayloadDigest == b.PayloadDigest && a.Body == b.Body
}

func (d *DB) CompleteManagedPRCommentCreate(pending PendingManagedPRComment, commentID string) error {
	if err := validatePendingManagedPRComment(pending); err != nil {
		return err
	}
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return fmt.Errorf("complete managed PR comment create: comment ID is required")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("complete managed PR comment create: %w", err)
	}
	defer tx.Rollback()
	var existing PendingManagedPRComment
	if err := tx.QueryRow(
		`SELECT repo_id, provider, pr_number, pr_url, head_sha, principal, marker_digest, payload_digest, body, created_at
		   FROM pending_managed_pr_comments
		  WHERE repo_id = ? AND provider = ? AND pr_number = ?`,
		pending.RepoID, pending.Provider, pending.PRNumber,
	).Scan(&existing.RepoID, &existing.Provider, &existing.PRNumber, &existing.PRURL, &existing.HeadSHA, &existing.Principal, &existing.MarkerDigest, &existing.PayloadDigest, &existing.Body, &existing.CreatedAt); err != nil {
		return fmt.Errorf("complete managed PR comment create: read pending intent: %w", err)
	}
	if !samePendingManagedPRComment(existing, pending) {
		return fmt.Errorf("complete managed PR comment create: pending intent changed")
	}
	if _, err := tx.Exec(
		`INSERT INTO managed_pr_comments (repo_id, provider, pr_number, pr_url, comment_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, provider, pr_number) DO NOTHING`,
		pending.RepoID, pending.Provider, pending.PRNumber, pending.PRURL, commentID, now(),
	); err != nil {
		return fmt.Errorf("complete managed PR comment create: bind identity: %w", err)
	}
	var boundID string
	if err := tx.QueryRow(
		`SELECT comment_id FROM managed_pr_comments WHERE repo_id = ? AND provider = ? AND pr_number = ?`,
		pending.RepoID, pending.Provider, pending.PRNumber,
	).Scan(&boundID); err != nil || boundID != commentID {
		return fmt.Errorf("complete managed PR comment create: conflicting bound identity")
	}
	if _, err := tx.Exec(
		`DELETE FROM pending_managed_pr_comments WHERE repo_id = ? AND provider = ? AND pr_number = ?`,
		pending.RepoID, pending.Provider, pending.PRNumber,
	); err != nil {
		return fmt.Errorf("complete managed PR comment create: clear pending intent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete managed PR comment create: %w", err)
	}
	return nil
}
