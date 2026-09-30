package forgejo

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestManagedPullCommentTransport(t *testing.T) {
	t.Parallel()
	body := "validation body"
	issueURL := testBaseURL + "/api/v1/repos/" + testRepo + "/issues/42"
	principal, _ := json.Marshal(map[string]any{"status": 200, "data": map[string]any{"id": 101}})
	list, _ := json.Marshal(map[string]any{"status": 200, "data": []any{map[string]any{"id": 11, "body": "old", "issue_url": issueURL, "user": map[string]any{"id": 101}}}})
	later, _ := json.Marshal(map[string]any{"status": 200, "data": []any{map[string]any{"id": 13, "body": "later page", "issue_url": issueURL, "user": map[string]any{"id": 101}}}})
	empty, _ := json.Marshal(map[string]any{"status": 200, "data": []any{}})
	created, _ := json.Marshal(map[string]any{"status": 201, "data": map[string]any{"id": 12, "body": body, "issue_url": issueURL, "user": map[string]any{"id": 101}}})
	updated, _ := json.Marshal(map[string]any{"status": 200, "data": map[string]any{"id": 11, "body": body, "issue_url": issueURL, "user": map[string]any{"id": 101}}})
	r := &fakeRecorder{responses: []fakeResponse{{stdout: string(principal)}, {stdout: string(list)}, {stdout: string(later)}, {stdout: string(empty)}, {stdout: string(created)}, {stdout: string(updated)}}}
	host := newTestHost(r)
	host.goos = "windows"
	pr := &scm.PR{Number: "42", URL: testPRURL}
	authenticated, err := host.AuthenticatedPRCommentPrincipal(context.Background())
	if err != nil || authenticated != "101" {
		t.Fatalf("AuthenticatedPRCommentPrincipal() = %q, %v", authenticated, err)
	}
	comments, err := host.ListPRComments(context.Background(), pr)
	if err != nil || len(comments) != 2 || comments[0].ID != "11" || comments[1].ID != "13" || comments[0].Principal != "101" || comments[1].Principal != "101" {
		t.Fatalf("ListPRComments() = %+v, %v", comments, err)
	}
	gotCreate, err := host.CreatePRComment(context.Background(), pr, body)
	if err != nil || gotCreate.ID != "12" {
		t.Fatalf("CreatePRComment() = %+v, %v", gotCreate, err)
	}
	gotUpdate, err := host.UpdatePRComment(context.Background(), pr, "11", body)
	if err != nil || gotUpdate.ID != "11" {
		t.Fatalf("UpdatePRComment() = %+v, %v", gotUpdate, err)
	}
	payload := `{"body":"validation body"}`
	want := [][]string{
		{"api", "GET", "user", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"api", "GET", "repos/" + testRepo + "/issues/42/comments?limit=50&page=1", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"api", "GET", "repos/" + testRepo + "/issues/42/comments?limit=50&page=2", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"api", "GET", "repos/" + testRepo + "/issues/42/comments?limit=50&page=3", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"api", "POST", "repos/" + testRepo + "/issues/42/comments", "--data", payload, "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"api", "PATCH", "repos/" + testRepo + "/issues/comments/11", "--data", payload, "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
	}
	for i := range want {
		if !reflect.DeepEqual(r.calls[i].args, want[i]) {
			t.Fatalf("call %d = %#v, want %#v", i, r.calls[i].args, want[i])
		}
	}
}

func TestManagedPullCommentRefusesOversizedWindowsCommandBeforeInvocation(t *testing.T) {
	t.Parallel()
	r := &fakeRecorder{}
	host := newTestHost(r)
	host.goos = "windows"
	pr := &scm.PR{Number: "42", URL: testPRURL}
	_, err := host.CreatePRComment(context.Background(), pr, strings.Repeat(`"`, scm.MaxManagedPRCommentBytes))
	if err == nil || !strings.Contains(err.Error(), "pr.appendix: minimal") || !strings.Contains(err.Error(), "Windows limit") {
		t.Fatalf("CreatePRComment() error = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("forgejo-axi calls = %d, want none", len(r.calls))
	}
}
