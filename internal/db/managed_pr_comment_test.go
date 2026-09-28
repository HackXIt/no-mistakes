package db

import "testing"

func TestManagedPRCommentPendingCreateCompletesAtomically(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.InsertRepoWithID("repo-1", t.TempDir(), "https://github.com/test/repo", "main"); err != nil {
		t.Fatal(err)
	}
	pending := PendingManagedPRComment{
		RepoID: "repo-1", Provider: "github", PRNumber: "42", PRURL: "https://github.com/test/repo/pull/42",
		HeadSHA: "head", Principal: "bot-1", MarkerDigest: "marker", PayloadDigest: "payload", Body: "body",
	}
	if err := database.BeginManagedPRCommentCreate(pending); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetPendingManagedPRComment("repo-1", "github", "42")
	if err != nil || got == nil || got.Principal != "bot-1" || got.Body != "body" {
		t.Fatalf("pending=%+v err=%v", got, err)
	}
	conflict := pending
	conflict.HeadSHA = "other"
	if err := database.BeginManagedPRCommentCreate(conflict); err == nil {
		t.Fatal("conflicting pending create intent was accepted")
	}
	if err := database.CompleteManagedPRCommentCreate(pending, "7"); err != nil {
		t.Fatal(err)
	}
	if got, err := database.GetPendingManagedPRComment("repo-1", "github", "42"); err != nil || got != nil {
		t.Fatalf("completed pending intent survived: %+v %v", got, err)
	}
	binding, err := database.GetManagedPRCommentBinding("repo-1", "github", "42")
	if err != nil || binding == nil || binding.CommentID != "7" {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if err := database.BeginManagedPRCommentCreate(pending); err == nil {
		t.Fatal("new pending intent was accepted after identity binding")
	}
}
