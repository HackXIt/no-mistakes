package gitlab

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestManagedMergeRequestCommentTransport(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "7", URL: "https://gitlab.example.com/group/project/-/merge_requests/7"}
	body := "validation body"
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --hostname gitlab.example.com user": {stdout: `{"id":101}`},
		"glab api --hostname gitlab.example.com --paginate projects/group%2Fproject/merge_requests/7/notes?per_page=100": {
			stdout: `[{"id":10,"body":"changed title","noteable_iid":7,"system":true},{"id":11,"body":"old","noteable_iid":7,"system":false,"author":{"id":101}}]`,
		},
		"glab api --hostname gitlab.example.com --method POST projects/group%2Fproject/merge_requests/7/notes --input -": {
			stdout: `{"id":12,"body":"validation body","noteable_iid":7,"system":false,"author":{"id":101}}`,
		},
		"glab api --hostname gitlab.example.com --method PUT projects/group%2Fproject/merge_requests/7/notes/11 --input -": {
			stdout: `{"id":11,"body":"validation body","noteable_iid":7,"system":false,"author":{"id":101}}`,
		},
	}), nil, "gitlab.example.com", "group/project")
	principal, err := host.AuthenticatedPRCommentPrincipal(context.Background())
	if err != nil || principal != "101" {
		t.Fatalf("AuthenticatedPRCommentPrincipal() = %q, %v", principal, err)
	}
	comments, err := host.ListPRComments(context.Background(), pr)
	if err != nil || len(comments) != 1 || comments[0].ID != "11" || comments[0].Principal != "101" {
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
