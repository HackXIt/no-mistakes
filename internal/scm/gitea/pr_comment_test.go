package gitea

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestManagedPullCommentTransport(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "7", URL: "https://gitea.example.com/owner/repo/pulls/7"}
	issueURL := "https://gitea.example.com/api/v1/repos/owner/repo/issues/7"
	body := "validation body"
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea api --login work /user": {stdout: `{"id":101}`},
		"tea api --login work /repos/owner/repo/issues/7/comments?limit=50&page=1": {
			stdout: `[{"id":11,"body":"old","issue_url":"` + issueURL + `","user":{"id":101}}]`,
		},
		"tea api --login work /repos/owner/repo/issues/7/comments?limit=50&page=2": {
			stdout: `[{"id":13,"body":"later page","issue_url":"` + issueURL + `","user":{"id":101}}]`,
		},
		"tea api --login work /repos/owner/repo/issues/7/comments?limit=50&page=3": {stdout: `[]`},
		"tea api --login work --method POST --field body=validation body /repos/owner/repo/issues/7/comments": {
			stdout: `{"id":12,"body":"validation body","issue_url":"` + issueURL + `","user":{"id":101}}`,
		},
		"tea api --login work --method PATCH --field body=validation body /repos/owner/repo/issues/comments/11": {
			stdout: `{"id":11,"body":"validation body","issue_url":"` + issueURL + `","user":{"id":101}}`,
		},
	}), nil, "gitea.example.com", "work", "owner/repo")
	principal, err := host.AuthenticatedPRCommentPrincipal(context.Background())
	if err != nil || principal != "101" {
		t.Fatalf("AuthenticatedPRCommentPrincipal() = %q, %v", principal, err)
	}
	comments, err := host.ListPRComments(context.Background(), pr)
	if err != nil || len(comments) != 2 || comments[0].ID != "11" || comments[1].ID != "13" || comments[0].Principal != "101" || comments[1].Principal != "101" {
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
