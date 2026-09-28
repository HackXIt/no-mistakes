package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestManagedPRCommentTransport(t *testing.T) {
	t.Parallel()
	body := "validation body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/owner/repo/pullrequests/7/comments":
			fmt.Fprint(w, `{"values":[{"id":11,"content":{"raw":"old"}}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/2.0/repositories/owner/repo/pullrequests/7/comments":
			var payload map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 12, "content": map[string]any{"raw": payload["content"]["raw"]}})
		case r.Method == http.MethodPut && r.URL.Path == "/2.0/repositories/owner/repo/pullrequests/7/comments/11":
			var payload map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "content": map[string]any{"raw": payload["content"]["raw"]}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	client, err := NewClientFromEnv([]string{envEmail + "=e", envToken + "=t", envAPIBaseURL + "=" + server.URL})
	if err != nil {
		t.Fatal(err)
	}
	host := NewHost(client, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)
	pr := &scm.PR{Number: "7", URL: "https://bitbucket.org/owner/repo/pull-requests/7"}
	comments, err := host.ListPRComments(context.Background(), pr)
	if err != nil || len(comments) != 1 || comments[0].ID != "11" {
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
