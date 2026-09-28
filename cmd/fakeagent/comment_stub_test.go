package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func runStubWithIO(t *testing.T, input string, fn func() int) string {
	t.Helper()
	oldIn, oldOut := os.Stdin, os.Stdout
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inWrite.WriteString(input); err != nil {
		t.Fatal(err)
	}
	inWrite.Close()
	os.Stdin, os.Stdout = inRead, outWrite
	defer func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		inRead.Close()
		outRead.Close()
	}()
	if code := fn(); code != 0 {
		t.Fatalf("stub exit code = %d", code)
	}
	outWrite.Close()
	out, err := io.ReadAll(outRead)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGitHubManagedCommentStubUsesAuthenticatedPrincipal(t *testing.T) {
	t.Setenv("FAKEAGENT_GH_COMMENT_FILE", filepath.Join(t.TempDir(), "comments.json"))
	principalJSON := runStubWithIO(t, "", func() int {
		return runGhForkPRStub([]string{"api", "--method", "GET", "user"})
	})
	var principal struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(principalJSON), &principal); err != nil || principal.ID != 101 {
		t.Fatalf("authenticated principal = %q, %v", principalJSON, err)
	}

	commentJSON := runStubWithIO(t, `{"body":"validation"}`, func() int {
		return runGhForkPRStub([]string{"api", "--method", "POST", "repos/owner/repo/issues/42/comments", "--input", "-"})
	})
	var comment ghManagedComment
	if err := json.Unmarshal([]byte(commentJSON), &comment); err != nil || comment.User.ID != principal.ID {
		t.Fatalf("created comment principal = %q, %v", commentJSON, err)
	}
}

func TestGiteaManagedCommentStubUsesAuthenticatedPrincipal(t *testing.T) {
	t.Setenv("FAKEAGENT_TEA_LOG", filepath.Join(t.TempDir(), "tea.log"))
	principalJSON := runStubWithIO(t, "", func() int {
		return runTeaStub([]string{"api", "--login", "e2e", "/user"})
	})
	var principal struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(principalJSON), &principal); err != nil || principal.ID != 101 {
		t.Fatalf("authenticated principal = %q, %v", principalJSON, err)
	}

	commentJSON := runStubWithIO(t, "", func() int {
		return runTeaStub([]string{"api", "--login", "e2e", "--method", "POST", "--field", "body=validation", "/repos/owner/repo/issues/42/comments"})
	})
	var comment teaManagedComment
	if err := json.Unmarshal([]byte(commentJSON), &comment); err != nil || comment.User.ID != principal.ID {
		t.Fatalf("created comment principal = %q, %v", commentJSON, err)
	}
}
