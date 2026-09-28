package steps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_GhNotAvailable(t *testing.T) {
	t.Parallel()
	// Verify the step skips gracefully when the required provider CLI is missing.
	if _, err := exec.LookPath("gh"); err == nil {
		// gh is available on this machine, so we can't force the missing-CLI path here.
		t.Skip("gh is available, skipping unavailable test")
	}

	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip when gh is unavailable, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
	if !outcome.Skipped {
		t.Fatal("expected skipped outcome when PR step skips")
	}
}

func TestPRStep_UpdatesExistingPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr edit was called to update the PR body
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr edit") {
		t.Errorf("expected gh pr edit to be called, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--body") {
		t.Errorf("expected --body flag in gh pr edit, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, noMistakesPRSignature) {
		t.Errorf("expected updated PR body to include no-mistakes signature, got:\n%s", ghLog)
	}

	// Verify PR URL was stored
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://github.com/test/repo/pull/42" {
		t.Errorf("PR URL = %v, want https://github.com/test/repo/pull/42", run.PRURL)
	}
}

func TestPRStep_MalformedPRListFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")
	env = append(env, "FAKE_CLI_PR_LIST_JSON=[{")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	_, err := (&PRStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("Execute() error = nil, want malformed PR-list error")
	}
	if !strings.Contains(err.Error(), "parse gh pr list JSON") {
		t.Fatalf("Execute() error = %v, want GitHub parse context", err)
	}

	logData, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr list --head feature ") || !strings.Contains(ghLog, "--state open --json number,url,baseRefName") {
		t.Fatalf("expected production PR lookup command, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "pr create") || strings.Contains(ghLog, "pr edit") {
		t.Fatalf("malformed lookup must stop before PR mutation, got:\n%s", ghLog)
	}

	run, readErr := sctx.DB.GetRun(sctx.Run.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if run.PRURL != nil {
		t.Fatalf("PR URL = %q, want nil after malformed lookup", *run.PRURL)
	}
	t.Logf("gh transcript:\n%sobserved pipeline error: %v\nstored PR URL: <nil>", ghLog, err)
}

func TestPRStep_UsesResolvedForgeProviderForSelfHostedRemote(t *testing.T) {
	t.Parallel()
	const credentialSentinel = "credential-must-not-enter-pr-artifacts"
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "https://code.example.test/test/repo/pull/42")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "GH_TOKEN="+credentialSentinel)
	sctx.Repo.UpstreamURL = "git@work-code:test/repo.git"
	sctx.ForgeContext = &forgecontext.Context{
		Provider: scm.ProviderGitHub,
		Host:     "code.example.test",
		Environment: runenv.Overlay{
			Set:   map[string]string{"GH_CONFIG_DIR": "/profiles/work"},
			Unset: []string{"GH_TOKEN"},
		},
	}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Skipped {
		t.Fatal("expected configured forge provider to handle an otherwise unknown remote host")
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "pr edit") {
		t.Fatalf("expected gh to update the existing PR, got:\n%s", logData)
	}
	if !strings.Contains(string(logData), "auth status --hostname code.example.test") {
		t.Fatalf("expected gh auth to use the frozen profile host, got:\n%s", logData)
	}
	if strings.Contains(string(logData), credentialSentinel) {
		t.Fatalf("credential sentinel leaked into provider arguments or PR content:\n%s", logData)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), credentialSentinel) {
		t.Fatalf("credential sentinel leaked into run record: %s", persisted)
	}
}

func TestPRStep_BitbucketUpdatesExistingPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if api.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", api.listCalls)
	}
	if api.updateCalls != 1 {
		t.Fatalf("update calls = %d, want 1", api.updateCalls)
	}
	if api.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", api.createCalls)
	}
	if api.lastAuthHeader == "" {
		t.Fatal("expected Authorization header for Bitbucket API")
	}
	if strings.Contains(api.lastUpdateBody, "title") || !strings.Contains(api.lastUpdateBody, "description") {
		t.Fatalf("expected author-safe Bitbucket update to omit the live title and update only the description, got %q", api.lastUpdateBody)
	}
	if api.commentWrites != 1 || !strings.Contains(api.commentBody, validationCommentHeading) {
		t.Fatalf("managed validation comment was not created exactly once: writes=%d body=%q", api.commentWrites, api.commentBody)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://bitbucket.org/test/repo/pull-requests/42" {
		t.Fatalf("PR URL = %v, want Bitbucket PR URL", run.PRURL)
	}
}

func TestPRStep_BitbucketUpdatesExistingPRWithoutHTMLLink(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42")
	api.existingPRURL = "https://bitbucket.org/test/repo/pull-requests/42"
	api.createdPRURL = ""

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if api.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", api.listCalls)
	}
	if api.updateCalls != 1 {
		t.Fatalf("update calls = %d, want 1", api.updateCalls)
	}
	if api.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", api.createCalls)
	}
	if outcome.PRURL != api.existingPRURL {
		t.Fatalf("outcome PR URL = %q, want %q", outcome.PRURL, api.existingPRURL)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != api.existingPRURL {
		t.Fatalf("PR URL = %v, want %q", run.PRURL, api.existingPRURL)
	}
}

func TestPRStep_ZeroBaseSHA(t *testing.T) {
	t.Parallel()
	// New branch scenario: baseSHA is all-zeros, commit log should still work
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{name: "test"}
	zeroSHA := "0000000000000000000000000000000000000000"
	sctx := newTestContextWithDBRecords(t, ag, dir, zeroSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr create was called (not blocked by zero SHA)
	logData, _ := os.ReadFile(logFile)
	if !strings.Contains(string(logData), "pr create") {
		t.Errorf("expected gh pr create, got:\n%s", logData)
	}
}

func TestPRStep_CreatesConfiguredDraftPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// No existing PR - pr view returns exit 1
	env, logFile := fakeGH(t, "")

	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.Providers.GitHub.DraftPullRequests = true
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, findings); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr create was called
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr create") {
		t.Errorf("expected gh pr create to be called, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pr create --head feature --base main") {
		t.Fatalf("expected unset PR base to fall back to repository default branch, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pr create --head feature --base main --repo test/repo --draft") {
		t.Fatalf("expected configured GitHub PR creation to use --draft, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--title chore: update pull request --body") {
		t.Fatalf("expected fallback PR title to make no scope claim, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--title feat: add feature") {
		t.Fatalf("expected fallback PR title to exclude commit-history scope, got:\n%s", ghLog)
	}
	comment, err := step.renderValidationComment(sctx, scm.ProviderGitHub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(comment, "## Risk Assessment\n\n⚠️ Medium: touches critical error handling") {
		t.Fatalf("expected validation comment to carry the recorded risk note, got:\n%s", comment)
	}
	descriptionLog, _, _ := strings.Cut(ghLog, "api --hostname")
	if strings.Contains(descriptionLog, "A\tfeature.txt") || strings.Contains(descriptionLog, "## Risk Assessment") {
		t.Fatalf("fallback description repeated diff or validation detail:\n%s", descriptionLog)
	}

	// Verify PR URL was stored
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://github.com/test/repo/pull/99" {
		t.Fatalf("PR URL = %v, want https://github.com/test/repo/pull/99", run.PRURL)
	}
	for _, line := range strings.Split(ghLog, "\n") {
		if strings.HasPrefix(line, "pr create ") {
			t.Logf("provider command: gh %s\npersisted PR URL: %s", line, *run.PRURL)
			break
		}
	}
}

func TestPRStep_UsesConfiguredBaseBranch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "develop", baseSHA)
	env, logFile := fakeGH(t, "")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.BaseBranch = "develop"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "pr list --head feature ") {
		t.Fatalf("expected PR lookup by branch, got:\n%s", logData)
	}
	if strings.Contains(string(logData), "pr list --head feature --base") {
		t.Fatalf("expected PR lookup not to filter by base branch (would miss an existing PR opened against a different base), got:\n%s", logData)
	}
	if !strings.Contains(string(logData), "pr create --head feature --base develop") {
		t.Fatalf("expected configured base branch in PR creation, got:\n%s", logData)
	}
}

// TestPRStep_ExistingPRAgainstDifferentBaseIsUpdatedNotDuplicated reproduces
// the bug where a maintainer changes pr.base_branch after a PR already exists
// against the old base. A base-filtered `gh pr list` would then miss that
// still-open PR (GitHub filters server-side), so the step fell through to
// `gh pr create` and opened a second, duplicate PR against the new base while
// orphaning the original.
func TestPRStep_ExistingPRAgainstDifferentBaseIsUpdatedNotDuplicated(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/42", "develop")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.BaseBranch = "main"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if strings.Contains(ghLog, "pr create") {
		t.Fatalf("expected existing PR to be updated, not duplicated with a new pr create, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pr edit") {
		t.Fatalf("expected gh pr edit to update the existing PR, got:\n%s", ghLog)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://github.com/test/repo/pull/42" {
		t.Errorf("PR URL = %v, want the existing PR to remain https://github.com/test/repo/pull/42", run.PRURL)
	}
}

// TestPRStep_SkipsWhenBranchMatchesConfiguredBaseBranch reproduces the
// 0530823 bug: with pr.base_branch configured to a branch other than the
// repo's forge default, pushing directly to that configured base branch must
// still skip PR creation instead of attempting a self-targeting PR. Before
// that fix, the skip check compared only against sctx.Repo.DefaultBranch, so
// a run on "develop" (configured base) would fall through to gh pr create.
func TestPRStep_SkipsWhenBranchMatchesConfiguredBaseBranch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.BaseBranch = "develop"
	sctx.Run.Branch = "refs/heads/develop"

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Skipped {
		t.Fatal("expected PR creation to be skipped when branch matches configured base branch")
	}

	if logData, err := os.ReadFile(logFile); err == nil {
		t.Fatalf("expected no gh invocation when branch matches configured base branch, got log:\n%s", logData)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPRStep_GitHubForkCreatesParentPRWithForkHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "develop", baseSHA)
	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "hosts.yml"), []byte("github.com:\n    user: fork-user\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env, logFile := fakeGH(t, "")
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: route fork prs","body":"## Summary\n\n- open fork PR against parent"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://github.com/parent-owner/no-mistakes.git"
	sctx.Repo.ForkURL = "https://github.com/fork-owner/no-mistakes.git"
	sctx.Config.PR.BaseBranch = "develop"
	sctx.Run.Branch = "refs/heads/feature"
	forgeCtx, err := forgecontext.Resolve(context.Background(), config.ForgeProfiles{
		"github.com": {GHConfigDir: profileDir},
	}, sctx.Repo.UpstreamURL, sctx.Repo.ForkURL)
	if err != nil {
		t.Fatal(err)
	}
	sctx.ForgeContext = forgeCtx

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr list --head feature --repo parent-owner/no-mistakes --state open --json number,url,baseRefName,headRefName,headRepositoryOwner") {
		t.Fatalf("expected PR lookup to use parent repo and bare head branch, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "pr list --head fork-owner:feature") {
		t.Fatalf("PR lookup used unsupported owner-qualified --head, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pr create --head fork-owner:feature --base develop --repo parent-owner/no-mistakes") {
		t.Fatalf("expected PR create to target parent repo with fork owner head, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--repo fork-owner/no-mistakes") {
		t.Fatalf("expected no self-PR against fork repo, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "pr create --head feature --") {
		t.Fatalf("expected PR create to avoid bare fork head, got:\n%s", ghLog)
	}
	if forgeCtx == nil || forgeCtx.ConfigDir != profileDir {
		t.Fatalf("fork PR used forge context %#v, want %s", forgeCtx, profileDir)
	}
}

func TestPRStep_BitbucketCreatesNewPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 0, "")

	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, findings); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if api.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", api.listCalls)
	}
	if api.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", api.createCalls)
	}
	if api.updateCalls != 0 {
		t.Fatalf("update calls = %d, want 0", api.updateCalls)
	}
	if !strings.Contains(api.lastCreateBody, `"source"`) || !strings.Contains(api.lastCreateBody, `"destination"`) {
		t.Fatalf("expected Bitbucket PR create payload to include source and destination, got %q", api.lastCreateBody)
	}
	description := bitbucketPRDescriptionForTest(t, api.lastCreateBody)
	for _, leak := range []string{"<details>", "<summary>", "<code>", "<video"} {
		if strings.Contains(description, leak) {
			t.Errorf("Bitbucket PR create shipped unsupported HTML %q:\n%s", leak, description)
		}
	}
	if !strings.Contains(description, "```text\n"+pipelineAttestationCommentPrefix) || strings.Count(description, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("Bitbucket description did not carry the one visible enforcement marker:\n%s", description)
	}
	if api.commentWrites != 1 || !strings.Contains(api.commentBody, validationCommentHeading) {
		t.Fatalf("Bitbucket validation comment was not maintained: writes=%d body=%q", api.commentWrites, api.commentBody)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != api.createdPRURL {
		t.Fatalf("PR URL = %v, want %q", run.PRURL, api.createdPRURL)
	}
}

func TestPRStep_BitbucketCreatesNewPRWithoutHTMLLink(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 0, "")
	api.createdPRURL = ""

	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, findings); err != nil {
		t.Fatal(err)
	}
	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if outcome.PRURL != "https://bitbucket.org/test/repo/pull-requests/99" {
		t.Fatalf("PR URL = %q, want derived Bitbucket PR URL", outcome.PRURL)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://bitbucket.org/test/repo/pull-requests/99" {
		t.Fatalf("PR URL = %v, want derived Bitbucket PR URL", run.PRURL)
	}
}

func TestPRStep_BitbucketMissingEnvSkipsBeforeBuildingContent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected Bitbucket PR step to skip before building content, got %d agent calls", len(ag.calls))
	}
}

func TestPRStep_BitbucketUsesProcessEnvWhenStepEnvIsNil(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 0, "")
	t.Setenv("NO_MISTAKES_BITBUCKET_EMAIL", "test@example.com")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_TOKEN", "test-token")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_BASE_URL", api.server.URL)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: process env bitbucket pr","body":"## Summary\n\n- create PR via process env"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if outcome.PRURL != api.createdPRURL {
		t.Fatalf("PR URL = %q, want %q", outcome.PRURL, api.createdPRURL)
	}
	if api.createCalls != 1 {
		t.Fatalf("expected Bitbucket PR create API to be called once, got %d", api.createCalls)
	}
}

func TestPRStep_UsesConfiguredTitleFormat(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "{{.Branch}}: {{.Title}}") {
				t.Error("prompt exposed configured title format as agent instructions")
			}
			if !strings.Contains(opts.Prompt, "only the bare concise title text") {
				t.Error("prompt did not request the bare title component")
			}
			payload := json.RawMessage(`{"title":"add widget","body":"## What Changed\n\n- add widget support"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.Branch = "refs/heads/PROJ/123"
	sctx.Config.Commit.BranchPattern = `^PROJ/([0-9]+)$`
	sctx.Config.Commit.BranchReplacement = "PROJ-${1}"
	sctx.Config.PR.TitleFormat = "{{.Branch}}: {{.Title}}"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "--title PROJ-123: add widget") {
		t.Fatalf("expected configured PR title, got:\n%s", logData)
	}
}

func TestPRStep_ConfiguredTitleRejectsBodyRepeatingBareTitle(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"title":"Add managed comments","body":"Add managed comments"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.TitleFormat = "[ABC] {{.Title}}"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(string(body))
	if err != nil || strings.TrimSpace(parts.before) != "Updates the final branch delta." {
		t.Fatalf("duplicate bare title was published instead of the concise fallback: %q (%v)", body, err)
	}
}

func TestPRStep_UnwrapsNestedJSONBody(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	// Agent returns body as the serialized prContent JSON (the bug LLMs sometimes produce).
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: improve pipeline header UX","body":"{\"title\":\"fix: improve pipeline header UX\",\"body\":\"## Summary\\n\\n- keep branch status readable\\n- fix footer truncation\"}"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)

	// The guard should unwrap the nested body and use the real markdown.
	if !strings.Contains(ghLog, "keep branch status readable") {
		t.Fatalf("expected unwrapped PR body in gh call, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, `"title"`) {
		t.Fatalf("expected JSON wrapper to be stripped from PR body, got:\n%s", ghLog)
	}
}

func TestUnwrapNestedPRBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "empty string", body: "", want: ""},
		{name: "plain markdown", body: "## Summary\n\n- bullet one", want: "## Summary\n\n- bullet one"},
		{name: "invalid JSON starting with brace", body: "{not valid json", want: "{not valid json"},
		{name: "valid JSON but empty nested body", body: `{"title":"fix: stuff","body":""}`, want: `{"title":"fix: stuff","body":""}`},
		{name: "nested JSON body is unwrapped", body: `{"title":"fix: stuff","body":"## Summary\n\n- real body"}`, want: "## Summary\n\n- real body"},
		{name: "nested JSON body with whitespace", body: `{"title":"fix: stuff","body":"  ## Summary  "}`, want: "## Summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unwrapNestedPRBody(tt.body)
			if got != tt.want {
				t.Errorf("unwrapNestedPRBody(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestPRBodyBudgetPromptSection(t *testing.T) {
	t.Parallel()
	if got := prBodyBudgetPromptSection(0); got != "" {
		t.Fatalf("prBodyBudgetPromptSection(0) = %q, want empty", got)
	}
	got := prBodyBudgetPromptSection(4000)
	if !strings.Contains(got, "4000 characters") || !strings.Contains(got, "compact machine trailer") || !strings.Contains(got, "three brief bullets") {
		t.Fatalf("prBodyBudgetPromptSection(4000) missing budget guidance: %q", got)
	}
}

func bitbucketPRDescriptionForTest(t *testing.T, raw string) string {
	t.Helper()
	var payload struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode Bitbucket PR payload: %v\n%s", err, raw)
	}
	if payload.Description == "" {
		t.Fatalf("Bitbucket PR payload missing description:\n%s", raw)
	}
	return payload.Description
}

func pipelineMarkdownForTest(rounds ...string) string {
	var b strings.Builder
	b.WriteString("## Pipeline\n\nUpdates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)\n\n")
	b.WriteString("<details>\n")
	b.WriteString("<summary>🔧 **Review** - update rounds</summary>\n\n")
	for _, round := range rounds {
		b.WriteString(round)
		b.WriteString("\n\n")
	}
	b.WriteString("</details>\n")
	return b.String()
}

func bitbucketPipelineMarkdownForTest(rounds ...string) string {
	var b strings.Builder
	b.WriteString("## Pipeline\n\nUpdates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)\n\n")
	b.WriteString("### ✅ **Review** - passed\n\n")
	for _, round := range rounds {
		b.WriteString(round)
		b.WriteString("\n\n")
	}
	return b.String()
}

func parsePipelineAttestationForTest(t *testing.T, body string) pipelineAttestation {
	t.Helper()
	start := strings.Index(body, pipelineAttestationCommentPrefix)
	if start < 0 {
		t.Fatalf("PR body missing pipeline attestation:\n%s", body)
	}
	start += len(pipelineAttestationCommentPrefix)
	end := strings.Index(body[start:], pipelineAttestationCommentClosingToken)
	if end < 0 {
		t.Fatalf("PR body contains unclosed pipeline attestation:\n%s", body)
	}
	var attestation pipelineAttestation
	if err := json.Unmarshal([]byte(body[start:start+end]), &attestation); err != nil {
		t.Fatalf("parse pipeline attestation: %v", err)
	}
	return attestation
}

func readFakeGHBodyArg(t *testing.T, logFile string) string {
	t.Helper()
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	const marker = " --body "
	log := string(logData)
	idx := strings.LastIndex(log, marker)
	if idx < 0 {
		t.Fatalf("expected fake gh log to include --body, got:\n%s", log)
	}
	return strings.TrimSuffix(log[idx+len(marker):], "\n")
}

func assertGitHubBodyLimitForTest(t *testing.T, body string) {
	t.Helper()
	if got := len(body); got >= githubPullRequestBodyHardLimitChars {
		t.Fatalf("body length = %d, want below GitHub hard limit %d", got, githubPullRequestBodyHardLimitChars)
	}
	if got := len(body); got > maxPullRequestBodyBytes {
		t.Fatalf("body length = %d, want safety buffer below %d", got, maxPullRequestBodyBytes)
	}
}

func assertNoPartialRoundLinesForTest(t *testing.T, body string, rounds []string) {
	t.Helper()
	full := make(map[string]struct{}, len(rounds))
	for _, round := range rounds {
		full[round] = struct{}{}
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "review round ") {
			continue
		}
		if _, ok := full[line]; !ok {
			t.Fatalf("pipeline update was truncated mid-line: %q", line)
		}
	}
}

func TestPRStep_OmitsIntentSectionWhenIntentEmpty(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"feat: add bar","body":"## What Changed\n\n- add Bar()"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.UserIntent = ""

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)

	if strings.Contains(ghLog, "## Intent") {
		t.Fatalf("expected no ## Intent section when intent is empty, got:\n%s", ghLog)
	}
}

func TestPRStep_GitLabCreatesNewMR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGlab(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"feat: improve gitlab flow","body":"## Summary\n\n- add gitlab support\n\n## Testing\n\n- go test ./..."}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "mr create") {
		t.Fatalf("expected glab mr create to be called, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--title feat: improve gitlab flow") {
		t.Fatalf("expected generated title in glab call, got:\n%s", ghLog)
	}
}

func TestPRStep_SkipsWhenProviderCLIUnavailable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Env = []string{"PATH=" + t.TempDir()}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip instead of failure, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL != nil {
		t.Fatalf("expected no PR URL when provider CLI unavailable, got %q", *run.PRURL)
	}
}

func TestPRStep_SkipsBeforeBuildingContentWhenProviderCLIUnavailable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("expected PR content generation to be skipped when CLI is unavailable")
			return nil, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Env = []string{"PATH=" + t.TempDir()}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip instead of failure, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no agent calls when provider CLI unavailable, got %d", len(ag.calls))
	}
}

func TestPRStep_AgentNonConventionalTitleFallsBack(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"Improve pipeline header UX","body":"## Summary\n\n- improvements"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	// The title should be prefixed with a release-triggering type, not the raw agent output.
	if strings.Contains(ghLog, "--title Improve pipeline header UX --") {
		t.Fatal("non-conventional agent title should have been rejected")
	}
	if !strings.Contains(ghLog, "fix: Improve pipeline header UX") {
		t.Fatal("expected user-facing agent title to be prefixed with fix:, got: " + ghLog)
	}
	// The summary heading is removed so the description can become a squash
	// commit body, while the substantive agent-authored bullet survives.
	if strings.Contains(ghLog, "## Summary") || !strings.Contains(ghLog, "- improvements") {
		t.Fatal("expected normalized concise agent body, got: " + ghLog)
	}
}

func TestPRStep_AgentScopedBreakingTitlePassesThrough(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	const title = "feat(api)!: require auth token"
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"feat(api)!: require auth token","body":"## Summary\n\n- require auth token on all API requests"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "--title "+title+" --body") {
		t.Fatalf("expected scoped conventional breaking-change title to pass through unchanged, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--title chore: "+title+" --body") {
		t.Fatalf("expected scoped conventional breaking-change title to avoid fallback prefix, got:\n%s", ghLog)
	}
}

func TestPRStep_AgentConventionalNonReleaseTitlePassesThrough(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"refactor(cli): improve CLI output","body":"## Summary\n\n- improve user-visible command output"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "--title refactor(cli): improve CLI output --body") {
		t.Fatalf("expected conventional agent PR title to pass through unchanged, got:\n%s", ghLog)
	}
}

func TestPRStep_PromptRequiresReleaseTypesForProductImpact(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: improve CLI output","body":"## What Changed\n\n- improve output"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &PRStep{}
	if _, err := step.buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "user-facing product impact") {
		t.Fatalf("prompt should mention user-facing product impact rule, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "must use feat or fix") {
		t.Fatalf("prompt should require feat or fix for product impact, got:\n%s", prompt)
	}
}

// TestPRStep_PromptGuidesScopeToRealModule verifies the PR prompt instructs
// the agent to pick a scope that is a real, primary, not-too-granular
// module/package name in the codebase.
func TestPRStep_PromptGuidesScopeToRealModule(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, _ := fakeGH(t, "")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			payload := json.RawMessage(`{"title":"fix(daemon): tidy logs","body":"## Summary\n\n- tidy"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(capturedPrompt, "real package/module name that exists in the codebase") {
		t.Errorf("expected PR prompt to require scope be a real package/module name in the codebase, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "primary module affected") {
		t.Errorf("expected PR prompt to require scope be the primary module affected, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "not too granular") {
		t.Errorf("expected PR prompt to warn scope should not be too granular, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "fewer than 10 distinct") {
		t.Errorf("expected PR prompt to convey typical module count heuristic, got:\n%s", capturedPrompt)
	}
}

func TestPRStep_HangingAgentFallsBackAfterTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "hanging-pr-agent",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Output: json.RawMessage(`{"title":"feat: late title","body":"## What Changed\n\n- late"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.AgentTimeout = 20 * time.Millisecond

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatalf("buildPRContent: %v", err)
	}
	if content.Title != "chore: update pull request" {
		t.Fatalf("title = %q, want fallback after timeout", content.Title)
	}
	if strings.Contains(content.Body, "late") {
		t.Fatalf("used late agent body after timeout: %s", content.Body)
	}
}

func TestPRStep_LateSuccessAfterTimeoutDoesNotUseAgentTitle(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "late-pr-agent",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Output: json.RawMessage(`{"title":"feat: should not ship","body":"## What Changed\n\n- should not ship"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.AgentTimeout = 20 * time.Millisecond

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatalf("buildPRContent: %v", err)
	}
	if content.Title == "feat: should not ship" {
		t.Fatal("late successful PR title was used after the deadline")
	}
}

// TestPRStep_EmbeddedAttestationDoesNotShadowTheRealOne guards the compliance
// check against the PR body's own evidence.
//
// require-no-mistakes reads the FIRST attestation comment in the body and binds
// its head_sha to the PR head. A step agent that captures a generated PR body
// as evidence embeds that body's attestation comment verbatim, carrying the
// evidence run's head_sha; because the Testing section precedes the Pipeline
// section, the embedded copy wins and the check fails a PR the pipeline did
// produce. Seen live on kunchenguid/no-mistakes#831, whose test evidence
// embedded three.
func assertFirstAttestationBindsHead(t *testing.T, body, headSHA string) {
	t.Helper()
	if n := strings.Count(body, pipelineAttestationCommentPrefix); n != 1 {
		t.Fatalf("expected exactly one parseable attestation marker, got %d:\n%s", n, body)
	}
	start := strings.Index(body, pipelineAttestationCommentPrefix)
	start += len(pipelineAttestationCommentPrefix)
	end := strings.Index(body[start:], pipelineAttestationCommentClosingToken)
	if end < 0 {
		t.Fatalf("attestation comment is not closed:\n%s", body)
	}
	var attestation pipelineAttestation
	if err := json.Unmarshal([]byte(body[start:start+end]), &attestation); err != nil {
		t.Fatalf("first attestation does not parse: %v", err)
	}
	if attestation.HeadSHA != headSHA {
		t.Fatalf("first attestation binds %q, want the run head %q", attestation.HeadSHA, headSHA)
	}
}

// TestPRStep_AgentBodyAttestationDoesNotShadowTheRealOne extends the guard to
// PR-drafting output. A body containing a pasted attestation is not a valid
// concise squash description, so publication uses the deterministic fallback
// and retains only the real compact enforcement marker.
func TestPRStep_AgentBodyAttestationDoesNotShadowTheRealOne(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	foreignSHA := strings.Repeat("e", 40)
	embedded := pipelineAttestationCommentPrefix +
		`{"head_sha":"` + foreignSHA + `","steps":[{"step":"review","status":"completed"}]}` +
		pipelineAttestationCommentClosingToken
	ag := &mockAgent{
		name: "pr",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload, err := json.Marshal(prContent{
				Title: "fix(pipeline): keep the attestation authoritative",
				Body:  "## What Changed\n\n- reuses an earlier PR body verbatim:\n\n" + embedded,
			})
			if err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(payload)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{TestingSummary: "Ran the focused suite."}), "")

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}

	assertFirstAttestationBindsHead(t, content.Body, sctx.Run.HeadSHA)
	if strings.Contains(content.Body, foreignSHA) || strings.Count(content.Body, pipelineAttestationCommentPrefix) != 1 || !strings.Contains(content.Body, "Updates the final branch delta.") {
		t.Fatalf("invalid agent body did not fall back to one authoritative attestation:\n%s", content.Body)
	}
}

// TestFallbackPRBodyAttestationDoesNotShadowTheRealOne covers the fallback
// body, whose What Changed section embeds the final diff verbatim.
