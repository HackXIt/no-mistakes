package github

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestManagedPRCommentTransport(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "7", URL: "https://github.com/owner/repo/pull/7"}
	issueURL := "https://api.github.com/repos/owner/repo/issues/7"
	body := "validation\nbody"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --hostname github.com --method GET user": {stdout: `{"id":101}`},
		"gh api --hostname github.com --paginate --method GET repos/owner/repo/issues/7/comments?per_page=100": {
			stdout: `[{"id":11,"body":"old","html_url":"https://github.com/owner/repo/pull/7#issuecomment-11","issue_url":"` + issueURL + `","user":{"id":101}}]`,
		},
		"gh api --hostname github.com --method POST repos/owner/repo/issues/7/comments --input -": {
			stdout:    `{"id":12,"body":"validation\nbody","issue_url":"` + issueURL + `","user":{"id":101}}`,
			wantStdin: `{"body":"validation\nbody"}`,
		},
		"gh api --hostname github.com --method PATCH repos/owner/repo/issues/comments/11 --input -": {
			stdout:    `{"id":11,"body":"validation\nbody","issue_url":"` + issueURL + `","user":{"id":101}}`,
			wantStdin: `{"body":"validation\nbody"}`,
		},
	}), func() bool { return true }, "github.com", "owner/repo")

	principal, err := host.AuthenticatedPRCommentPrincipal(context.Background())
	if err != nil || principal != "101" {
		t.Fatalf("AuthenticatedPRCommentPrincipal() = %q, %v", principal, err)
	}
	comments, err := host.ListPRComments(context.Background(), pr)
	if err != nil || len(comments) != 1 || comments[0].ID != "11" || comments[0].Body != "old" || comments[0].Principal != "101" {
		t.Fatalf("ListPRComments() = %+v, %v", comments, err)
	}
	created, err := host.CreatePRComment(context.Background(), pr, body)
	if err != nil || created.ID != "12" || created.Body != body {
		t.Fatalf("CreatePRComment() = %+v, %v", created, err)
	}
	updated, err := host.UpdatePRComment(context.Background(), pr, "11", body)
	if err != nil || updated.ID != "11" || updated.Body != body {
		t.Fatalf("UpdatePRComment() = %+v, %v", updated, err)
	}
}

func TestManagedPRCommentRefusesSiblingRepositoryURL(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(nil), func() bool { return true }, "github.com", "owner/repo")
	_, err := host.ListPRComments(context.Background(), &scm.PR{Number: "7", URL: "https://github.com/owner/sibling/pull/7"})
	if err == nil {
		t.Fatal("sibling repository URL was accepted")
	}
}
