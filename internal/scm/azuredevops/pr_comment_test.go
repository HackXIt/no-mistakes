package azuredevops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestManagedPRCommentTransport(t *testing.T) {
	t.Parallel()
	var calls []string
	var payloads []string
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, joined)
		for i, arg := range args {
			if arg == "--in-file" && i+1 < len(args) {
				data, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Fatalf("read comment payload: %v", err)
				}
				payloads = append(payloads, string(data))
			}
		}
		response := ""
		switch {
		case strings.Contains(joined, "--resource pullRequestThreads") && strings.Contains(joined, "--http-method GET"):
			response = `{"count":1,"value":[{"id":21,"comments":[{"id":9,"parentCommentId":0,"content":"provider event","commentType":2,"isDeleted":false},{"id":1,"parentCommentId":0,"content":"old","commentType":1,"isDeleted":false}]}]}`
		case strings.Contains(joined, "--resource pullRequestThreads") && strings.Contains(joined, "--http-method POST"):
			response = `{"id":22,"comments":[{"id":1,"parentCommentId":0,"content":"validation body","commentType":1,"isDeleted":false}]}`
		case strings.Contains(joined, "--resource pullRequestThreadComments") && strings.Contains(joined, "--http-method PATCH"):
			response = `{"id":1,"parentCommentId":0,"content":"validation body","commentType":1,"isDeleted":false}`
		default:
			response = `{}`
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzureCommentHelperProcess", "--", response)
		cmd.Env = append(os.Environ(), "AZURE_COMMENT_HELPER=1")
		return cmd
	}
	host := New(factory, func() bool { return true }, testOrg, testProject, testRepo)
	pr := &scm.PR{Number: "7"}
	comments, err := host.ListPRComments(context.Background(), pr)
	if err != nil || len(comments) != 1 || comments[0].ID != "21:1" {
		t.Fatalf("ListPRComments() = %+v, %v", comments, err)
	}
	created, err := host.CreatePRComment(context.Background(), pr, "validation body")
	if err != nil || created.ID != "22:1" {
		t.Fatalf("CreatePRComment() = %+v, %v", created, err)
	}
	updated, err := host.UpdatePRComment(context.Background(), pr, "21:1", "validation body")
	if err != nil || updated.ID != "21:1" {
		t.Fatalf("UpdatePRComment() = %+v, %v", updated, err)
	}
	if len(calls) != 3 || !strings.Contains(calls[0], "pullRequestId=7") || !strings.Contains(calls[2], "threadId=21 commentId=1") {
		t.Fatalf("comment routes = %#v", calls)
	}
	if len(payloads) != 2 || !strings.Contains(payloads[0], `"validation body"`) || !strings.Contains(payloads[1], `"validation body"`) {
		t.Fatalf("comment payloads = %#v", payloads)
	}
}

func TestAzureCommentHelperProcess(t *testing.T) {
	if os.Getenv("AZURE_COMMENT_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			fmt.Fprint(os.Stdout, os.Args[i+1])
			os.Exit(0)
		}
	}
	os.Exit(1)
}
