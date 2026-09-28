package db

import "testing"

func TestManagedPRCommentBindingIsDurableAndImmutable(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.InsertRepoWithID("repo-1", t.TempDir(), "https://github.com/test/repo", "main"); err != nil {
		t.Fatal(err)
	}
	binding := ManagedPRCommentBinding{
		RepoID: "repo-1", Provider: "github", PRNumber: "42",
		PRURL: "https://github.com/test/repo/pull/42", CommentID: "7",
	}
	if err := database.BindManagedPRComment(binding); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetManagedPRCommentBinding("repo-1", "github", "42")
	if err != nil || got == nil || got.CommentID != "7" || got.PRURL != binding.PRURL {
		t.Fatalf("binding=%+v err=%v", got, err)
	}
	if err := database.BindManagedPRComment(binding); err != nil {
		t.Fatalf("idempotent binding failed: %v", err)
	}
	binding.CommentID = "8"
	if err := database.BindManagedPRComment(binding); err == nil {
		t.Fatal("managed comment identity was replaced")
	}
}
