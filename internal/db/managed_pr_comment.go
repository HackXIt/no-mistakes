package db

import (
	"database/sql"
	"fmt"
	"strings"
)

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

func (d *DB) BindManagedPRComment(binding ManagedPRCommentBinding) error {
	binding.RepoID = strings.TrimSpace(binding.RepoID)
	binding.Provider = strings.TrimSpace(binding.Provider)
	binding.PRNumber = strings.TrimSpace(binding.PRNumber)
	binding.PRURL = strings.TrimSpace(binding.PRURL)
	binding.CommentID = strings.TrimSpace(binding.CommentID)
	if binding.RepoID == "" || binding.Provider == "" || binding.PRNumber == "" || binding.PRURL == "" || binding.CommentID == "" {
		return fmt.Errorf("bind managed PR comment: repo, provider, PR number, PR URL and comment ID are required")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("bind managed PR comment: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO managed_pr_comments (repo_id, provider, pr_number, pr_url, comment_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, provider, pr_number) DO NOTHING`,
		binding.RepoID, binding.Provider, binding.PRNumber, binding.PRURL, binding.CommentID, now(),
	); err != nil {
		return fmt.Errorf("bind managed PR comment: %w", err)
	}
	var existingID string
	if err := tx.QueryRow(
		`SELECT comment_id FROM managed_pr_comments
		  WHERE repo_id = ? AND provider = ? AND pr_number = ?`,
		binding.RepoID, binding.Provider, binding.PRNumber,
	).Scan(&existingID); err != nil {
		return fmt.Errorf("bind managed PR comment: verify binding: %w", err)
	}
	if existingID != binding.CommentID {
		return fmt.Errorf("bind managed PR comment: review object already owns comment %s", existingID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bind managed PR comment: %w", err)
	}
	return nil
}
