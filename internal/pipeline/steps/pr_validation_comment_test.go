package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type managedCommentTestHost struct {
	scm.Host
	comments        []scm.PRComment
	principal       string
	createErr       error
	createBeforeErr bool
	creates         int
	createdBodies   []string
	updates         int
	listCalls       int
	hiddenListCalls map[int]bool
}

func (h *managedCommentTestHost) Provider() scm.Provider { return scm.ProviderGitHub }
func (h *managedCommentTestHost) AuthenticatedPRCommentPrincipal(context.Context) (string, error) {
	if h.principal == "" {
		return "bot-1", nil
	}
	return h.principal, nil
}
func (h *managedCommentTestHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{ManagedPRComments: true}
}
func (h *managedCommentTestHost) ListPRComments(context.Context, *scm.PR) ([]scm.PRComment, error) {
	h.listCalls++
	if h.hiddenListCalls[h.listCalls] {
		var visible []scm.PRComment
		for _, comment := range h.comments {
			if _, owned, _ := parseValidationComment(comment); !owned {
				visible = append(visible, comment)
			}
		}
		return visible, nil
	}
	return append([]scm.PRComment(nil), h.comments...), nil
}
func (h *managedCommentTestHost) CreatePRComment(_ context.Context, _ *scm.PR, body string) (scm.PRComment, error) {
	h.creates++
	h.createdBodies = append(h.createdBodies, body)
	principal, _ := h.AuthenticatedPRCommentPrincipal(context.Background())
	comment := scm.PRComment{ID: "7", Body: body, Principal: principal}
	if h.createErr != nil {
		err := h.createErr
		h.createErr = nil
		if h.createBeforeErr {
			h.comments = append(h.comments, comment)
		}
		return scm.PRComment{}, err
	}
	h.comments = append(h.comments, comment)
	return comment, nil
}
func (h *managedCommentTestHost) UpdatePRComment(_ context.Context, _ *scm.PR, id, body string) (scm.PRComment, error) {
	h.updates++
	for i := range h.comments {
		if h.comments[i].ID == id {
			h.comments[i].Body = body
			return h.comments[i], nil
		}
	}
	return scm.PRComment{}, scm.ErrUnsupported
}

func managedCommentContext(t *testing.T, url string) *pipeline.StepContext {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.InsertRepoWithID("repo-1", t.TempDir(), "https://github.com/test/repo", "main"); err != nil {
		t.Fatal(err)
	}
	return &pipeline.StepContext{
		Ctx: context.Background(),
		Run: &db.Run{ID: "run-1", RepoID: "repo-1", Branch: "feature", HeadSHA: "head-1", PRURL: &url},
		DB:  database,
	}
}

func TestWrapValidationCommentNeutralizesEmbeddedOwnershipMarkers(t *testing.T) {
	embedded := "captured comment:\n" + validationCommentStart + strings.Repeat("0", 64) + " -->\nold evidence\n" + validationCommentEnd
	body := wrapValidationComment(embedded)
	if matches := validationCommentMarkerPattern.FindAllStringIndex(body, -1); len(matches) != 2 {
		t.Fatalf("wrapped comment has %d live ownership markers, want 2:\n%s", len(matches), body)
	}
	owned, ok, err := parseValidationComment(scm.PRComment{ID: "1", Body: body})
	if err != nil || !ok {
		t.Fatalf("wrapped comment is not parseable: ok=%v err=%v\n%s", ok, err, body)
	}
	if !strings.Contains(owned.content, "captured comment:") || validationCommentMarkerPattern.MatchString(owned.content) {
		t.Fatalf("embedded evidence was lost or retained live ownership markers:\n%s", owned.content)
	}
}

func TestPublishValidationCommentCreatesThenUpdatesInPlaceAndPreservesHumanComments(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	host := &managedCommentTestHost{comments: []scm.PRComment{{ID: "human", Body: "maintainer note"}}}
	pr := &scm.PR{Number: "42", URL: url}
	sctx := managedCommentContext(t, url)
	first := wrapValidationComment("first validation")
	if err := publishValidationComment(sctx, host, pr, first); err != nil {
		t.Fatal(err)
	}
	if err := publishValidationComment(sctx, host, pr, first); err != nil {
		t.Fatal(err)
	}
	sctx.Run = &db.Run{ID: "run-2", RepoID: "repo-1", Branch: "feature", HeadSHA: "head-1", PRURL: &url}
	forged := wrapValidationComment("forged validation")
	malformed := strings.Replace(forged, "forged", "edited", 1)
	host.comments = append(host.comments,
		scm.PRComment{ID: "forged", Body: forged},
		scm.PRComment{ID: "malformed", Body: malformed},
	)
	second := wrapValidationComment("second validation")
	if err := publishValidationComment(sctx, host, pr, second); err != nil {
		t.Fatal(err)
	}
	if host.creates != 1 || host.updates != 1 || len(host.comments) != 4 {
		t.Fatalf("creates=%d updates=%d comments=%+v", host.creates, host.updates, host.comments)
	}
	if host.comments[0].Body != "maintainer note" || host.comments[1].ID != "7" || host.comments[1].Body != second || host.comments[2].Body != forged || host.comments[3].Body != malformed {
		t.Fatalf("persisted comment identity was not preserved: %+v", host.comments)
	}
}

func TestPublishValidationCommentSettlesDelayedCreateVisibilityWithoutDuplicating(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	host := &managedCommentTestHost{hiddenListCalls: map[int]bool{2: true}}
	body := wrapValidationComment("validation")
	if err := publishValidationComment(managedCommentContext(t, url), host, &scm.PR{Number: "42", URL: url}, body); err != nil {
		t.Fatal(err)
	}
	if host.creates != 1 || host.updates != 0 || len(host.comments) != 1 {
		t.Fatalf("creates=%d updates=%d comments=%+v", host.creates, host.updates, host.comments)
	}
	if host.listCalls < 3 {
		t.Fatalf("list calls=%d, want a bounded settlement re-read", host.listCalls)
	}
}

func TestPublishValidationCommentRecoversEveryPendingCreateBoundary(t *testing.T) {
	for _, mode := range []string{"before remote create", "after remote create", "uncertain create found", "uncertain create absent"} {
		t.Run(mode, func(t *testing.T) {
			url := "https://github.com/test/repo/pull/42"
			pr := &scm.PR{Number: "42", URL: url}
			sctx := managedCommentContext(t, url)
			host := &managedCommentTestHost{}
			body := wrapValidationComment("validation")
			expected, err := pendingValidationComment(sctx, host, pr, "42", "bot-1", body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "before") || strings.HasPrefix(mode, "after") {
				if err := sctx.DB.BeginManagedPRCommentCreate(expected); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "after remote create" {
				host.comments = append(host.comments, scm.PRComment{ID: "7", Body: body, Principal: "bot-1"})
				host.hiddenListCalls = map[int]bool{1: true}
			}
			if strings.HasPrefix(mode, "uncertain create") {
				host.createErr = errors.New("uncertain create")
				host.createBeforeErr = mode == "uncertain create found"
			}
			if err := publishValidationComment(sctx, host, pr, body); err != nil {
				t.Fatal(err)
			}
			wantCreates := 1
			if mode == "after remote create" {
				wantCreates = 0
			} else if mode == "uncertain create absent" {
				wantCreates = 2
			}
			if host.creates != wantCreates || len(host.comments) != 1 {
				t.Fatalf("mode=%s creates=%d comments=%+v", mode, host.creates, host.comments)
			}
			binding, err := sctx.DB.GetManagedPRCommentBinding("repo-1", "github", "42")
			if err != nil || binding == nil || binding.CommentID != "7" {
				t.Fatalf("mode=%s binding=%+v err=%v", mode, binding, err)
			}
		})
	}
}

func TestPublishValidationCommentSettlesMatchedOldPendingBodyBeforeCurrentUpdate(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	pr := &scm.PR{Number: "42", URL: url}
	sctx := managedCommentContext(t, url)
	host := &managedCommentTestHost{}
	oldBody := wrapValidationComment("old validation")
	pending, err := pendingValidationComment(sctx, host, pr, "42", "bot-1", oldBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.BeginManagedPRCommentCreate(pending); err != nil {
		t.Fatal(err)
	}
	host.comments = []scm.PRComment{{ID: "7", Body: oldBody, Principal: "bot-1"}}
	host.hiddenListCalls = map[int]bool{1: true}
	sctx.Run.HeadSHA = "head-2"
	newBody := wrapValidationComment("new validation")
	if err := publishValidationComment(sctx, host, pr, newBody); err != nil {
		t.Fatal(err)
	}
	if host.creates != 0 || host.updates != 1 || len(host.comments) != 1 || host.comments[0].Body != newBody {
		t.Fatalf("creates=%d updates=%d comments=%+v", host.creates, host.updates, host.comments)
	}
}

func TestPublishValidationCommentRetriesOnlyCurrentTightenedPolicyBody(t *testing.T) {
	const intentSecret = "private acquisition intent"
	const evidenceSecret = "private full evidence"
	for _, tc := range []struct {
		name     string
		withheld string
		tighten  func(*pipeline.StepContext)
	}{
		{name: "no publish intent", withheld: intentSecret, tighten: func(sctx *pipeline.StepContext) { sctx.Run.OmitIntent = true }},
		{name: "minimal appendix", withheld: evidenceSecret, tighten: func(sctx *pipeline.StepContext) { sctx.Config.PR.Appendix = config.PRAppendixMinimal }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.UserIntent = intentSecret
			insertCompletedStep(t, sctx, "review", `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"policy changed"}`, "")
			insertCompletedStep(t, sctx, "test", `{"findings":[],"summary":"clean","testing_summary":"`+evidenceSecret+`","tested":["focused"]}`, "")
			step := &PRStep{}
			oldBody, err := step.renderValidationComment(sctx, scm.ProviderGitHub)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(oldBody, tc.withheld) {
				t.Fatalf("old publication did not contain withheld fixture %q:\n%s", tc.withheld, oldBody)
			}
			host := &managedCommentTestHost{}
			pr := &scm.PR{Number: "42", URL: url}
			pending, err := pendingValidationComment(sctx, host, pr, "42", "bot-1", oldBody)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.BeginManagedPRCommentCreate(pending); err != nil {
				t.Fatal(err)
			}
			tc.tighten(sctx)
			currentBody, err := step.renderValidationComment(sctx, scm.ProviderGitHub)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(currentBody, tc.withheld) {
				t.Fatalf("tightened publication retained %q:\n%s", tc.withheld, currentBody)
			}
			if err := publishValidationComment(sctx, host, pr, currentBody); err != nil {
				t.Fatal(err)
			}
			if host.creates != 1 || host.updates != 0 || len(host.createdBodies) != 1 || host.createdBodies[0] != currentBody || strings.Contains(host.createdBodies[0], tc.withheld) {
				t.Fatalf("created stale policy body: creates=%d updates=%d bodies=%q", host.creates, host.updates, host.createdBodies)
			}
		})
	}
}

func TestPublishValidationCommentRecoveryRequiresOnePrincipalMatch(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	pr := &scm.PR{Number: "42", URL: url}
	body := wrapValidationComment("validation")
	for _, comments := range [][]scm.PRComment{
		{{ID: "wrong-author", Body: body, Principal: "human"}},
		{{ID: "7", Body: body, Principal: "bot-1"}, {ID: "8", Body: body, Principal: "bot-1"}},
	} {
		sctx := managedCommentContext(t, url)
		host := &managedCommentTestHost{comments: comments}
		if len(comments) > 1 {
			host.hiddenListCalls = map[int]bool{1: true}
		}
		expected, err := pendingValidationComment(sctx, host, pr, "42", "bot-1", body)
		if err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.BeginManagedPRCommentCreate(expected); err != nil {
			t.Fatal(err)
		}
		err = publishValidationComment(sctx, host, pr, body)
		if len(comments) == 1 {
			if err != nil || host.creates != 1 {
				t.Fatalf("wrong-principal comment blocked safe create: err=%v creates=%d", err, host.creates)
			}
		} else if err == nil || host.creates != 0 {
			t.Fatalf("ambiguous principal matches were accepted: err=%v creates=%d", err, host.creates)
		}
	}
}

func TestPublishValidationCommentRefusesChangedAuthenticatedPrincipal(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	pr := &scm.PR{Number: "42", URL: url}
	body := wrapValidationComment("validation")
	sctx := managedCommentContext(t, url)
	host := &managedCommentTestHost{}
	expected, err := pendingValidationComment(sctx, host, pr, "42", "bot-1", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.BeginManagedPRCommentCreate(expected); err != nil {
		t.Fatal(err)
	}
	host.principal = "bot-2"
	if err := publishValidationComment(sctx, host, pr, body); err == nil || host.creates != 0 {
		t.Fatalf("changed authenticated principal was accepted: err=%v creates=%d", err, host.creates)
	}
}

func TestPublishValidationCommentRefusesDuplicateOrEditedOwnership(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	pr := &scm.PR{Number: "42", URL: url}
	sctx := managedCommentContext(t, url)
	owned := wrapValidationComment("validation")
	for _, comments := range [][]scm.PRComment{
		{{ID: "1", Body: owned}, {ID: "2", Body: owned}},
		{{ID: "1", Body: strings.Replace(owned, "validation", "human edit", 1)}},
	} {
		host := &managedCommentTestHost{comments: comments}
		if err := publishValidationComment(sctx, host, pr, wrapValidationComment("new")); err == nil {
			t.Fatalf("ambiguous ownership was accepted: %+v", comments)
		}
		if host.creates != 0 || host.updates != 0 {
			t.Fatalf("refusal wrote a comment: creates=%d updates=%d", host.creates, host.updates)
		}
	}
}

func TestPublishValidationCommentRefusesMissingOrEditedBoundComment(t *testing.T) {
	for _, mode := range []string{"missing", "edited"} {
		t.Run(mode, func(t *testing.T) {
			url := "https://github.com/test/repo/pull/42"
			pr := &scm.PR{Number: "42", URL: url}
			sctx := managedCommentContext(t, url)
			host := &managedCommentTestHost{}
			first := wrapValidationComment("first validation")
			if err := publishValidationComment(sctx, host, pr, first); err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				host.comments = nil
			} else {
				host.comments[0].Body = strings.Replace(first, "first", "human edit", 1)
			}
			if err := publishValidationComment(sctx, host, pr, wrapValidationComment("second validation")); err == nil {
				t.Fatalf("%s bound comment was accepted", mode)
			}
			if host.creates != 1 || host.updates != 0 {
				t.Fatalf("%s bound comment caused another write: creates=%d updates=%d", mode, host.creates, host.updates)
			}
		})
	}
}

func TestPublishValidationCommentRefusesSiblingReviewObject(t *testing.T) {
	ownedURL := "https://github.com/test/repo/pull/42"
	ownedPR := &scm.PR{Number: "42", URL: ownedURL}
	sctx := managedCommentContext(t, ownedURL)
	body := wrapValidationComment("validation")
	host := &managedCommentTestHost{comments: []scm.PRComment{{ID: "7", Body: body, Principal: "bot-1"}}}
	pending, err := pendingValidationComment(sctx, host, ownedPR, "42", "bot-1", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.BeginManagedPRCommentCreate(pending); err != nil {
		t.Fatal(err)
	}
	err = publishValidationComment(sctx, host, &scm.PR{Number: "43", URL: "https://github.com/test/repo/pull/43"}, body)
	if err == nil || host.creates != 0 {
		t.Fatalf("sibling review object was not refused: err=%v creates=%d", err, host.creates)
	}
}

func TestPRPublicationSeparatesConciseDescriptionFromDetailedValidation(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	var prompt string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompt = opts.Prompt
		return &agent.Result{Output: json.RawMessage(`{"title":"fix: concise publication","body":"## What Changed\n\n- Keep descriptions concise.\n- Maintain one validation comment."}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.UserIntent = "Make publication concise without weakening validation."
	insertCompletedStep(t, sctx, "review", `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"publication contract changed"}`, "")
	insertCompletedStep(t, sctx, "test", `{"findings":[],"summary":"clean","testing_summary":"Provider contract tests passed.","tested":["go test ./..."]}`, "")
	insertCompletedStep(t, sctx, "document", `{"findings":[],"summary":"clean"}`, "")
	step := &PRStep{}
	content, err := step.buildPRContent(sctx, "feature", "main", base, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	comment, err := step.renderValidationComment(sctx, scm.ProviderGitHub)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"## What Changed", "## Intent", "## Risk Assessment", "## Testing", "## Pipeline", "go test ./...", "Provider contract tests passed"} {
		if strings.Contains(content.Body, forbidden) {
			t.Fatalf("description contains detailed or duplicate content %q:\n%s", forbidden, content.Body)
		}
	}
	if !strings.Contains(content.Body, "Keep descriptions concise") || strings.Count(content.Body, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("description lost narrative or compact attestation:\n%s", content.Body)
	}
	for _, want := range []string{validationCommentHeading, validationCommentPublishedHead + ": `" + head + "`", "## Intent", "## Risk Assessment", "## Testing", "## Pipeline", "go test ./...", "Provider contract tests passed"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("validation comment missing %q:\n%s", want, comment)
		}
	}
	if strings.Contains(comment, validationCommentHeadLabel+": `"+head+"`") {
		t.Fatalf("validation comment labelled a head without test provenance as validated:\n%s", comment)
	}
	if strings.Contains(comment, pipelineAttestationCommentPrefix) {
		t.Fatalf("validation comment duplicated the enforcement marker:\n%s", comment)
	}
	if _, ok, err := parseValidationComment(scm.PRComment{ID: "1", Body: comment}); err != nil || !ok {
		t.Fatalf("validation comment ownership was not parseable: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(prompt, "concise squash-commit-style description") || strings.Contains(prompt, `a "## What Changed" section`) {
		t.Fatalf("draft prompt did not own the concise description contract:\n%s", prompt)
	}
	if got, out := runVerifyPy(t, content.Body, head); got != "success" {
		t.Fatalf("body-based enforcement contract changed: %s\n%s", got, out)
	}
}

func TestPRStep_RefreshesGeneratedNarrativeAcrossHeads(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ := fakeGH(t, "https://github.com/test/repo/pull/42")
	env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile, "FAKE_CLI_PR_TITLE=feat: stable generated title")

	calls := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		body := "First generated narrative."
		if calls >= 2 {
			body = "Second generated narrative."
		}
		payload, err := json.Marshal(prContent{Title: "feat: stable generated title", Body: body})
		if err != nil {
			return nil, err
		}
		return &agent.Result{Output: payload}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Env = env
	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "second.txt")
	gitCmd(t, dir, "commit", "-m", "second delta")
	secondHead := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx.Run.HeadSHA = secondHead
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, secondHead); err != nil {
		t.Fatal(err)
	}
	firstBody, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	restamped, rebound, err := rebindOwnedPRAttestation(string(firstBody), secondHead, nil, pipelineAttestationPolicy{})
	if err != nil || !rebound {
		t.Fatalf("pre-push restamp failed: rebound=%v err=%v", rebound, err)
	}
	if err := os.WriteFile(bodyFile, []byte(restamped), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !strings.Contains(string(body), "Second generated narrative.") || strings.Contains(string(body), "First generated narrative.") {
		t.Fatalf("second head retained stale generated content: calls=%d body=%s", calls, body)
	}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("each PR execution must redraft still-owned content: calls=%d", calls)
	}
	parts, err := parsePROwnedBody(string(body))
	if err != nil || !parts.generatedNarrative || parts.generatedTitle {
		t.Fatalf("refreshed narrative or preserved title has wrong ownership: parts=%+v err=%v", parts, err)
	}
}

func TestPRStep_PreservesExistingHumanOwnedNarrative(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	author := "Human-owned release narrative.\n\nCloses test/repo#7\n"
	if err := os.WriteFile(bodyFile, []byte(author), 0o644); err != nil {
		t.Fatal(err)
	}
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile, "FAKE_CLI_PR_TITLE=Human-owned title")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("human-owned untemplated content must not be redrafted")
		return nil, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Env = env
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(string(body))
	if err != nil || strings.TrimSpace(parts.before) != strings.TrimSpace(author) || parts.generatedNarrative || parts.generatedTitle {
		t.Fatalf("human-owned narrative changed ownership: parts=%+v err=%v body=%s", parts, err, body)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "--title") {
		t.Fatalf("human-owned title was rewritten:\n%s", logData)
	}
}

func TestNormalizeSquashDescriptionRejectsLongFormOrDuplicateShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		body  string
		title string
		want  string
		valid bool
	}{
		{name: "paragraph", body: "A concise branch summary.", title: "fix: concise publication", want: "A concise branch summary.", valid: true},
		{name: "heading and bullets", body: "## What Changed\n\n* first\n* second\n* third", title: "fix: concise publication", want: "- first\n- second\n- third", valid: true},
		{name: "too many bullets", body: "- one\n- two\n- three\n- four", title: "fix: concise publication"},
		{name: "multiple paragraphs", body: "First paragraph.\n\nSecond paragraph.", title: "fix: concise publication"},
		{name: "command fence", body: "```text\ngo test ./...\n```", title: "fix: concise publication"},
		{name: "repeats title", body: "fix: concise publication", title: "fix: concise publication"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, valid := normalizeSquashDescription(tt.body, tt.title)
			if valid != tt.valid || got != tt.want {
				t.Fatalf("normalizeSquashDescription() = %q, %v; want %q, %v", got, valid, tt.want, tt.valid)
			}
		})
	}
}

func TestFitValidationCommentKeepsMarkdownBalancedWithinBudget(t *testing.T) {
	t.Parallel()
	var rounds []string
	for i := 0; i < 40; i++ {
		rounds = append(rounds, strings.Repeat("recorded validation detail ", 40))
	}
	testing := "## Testing\n\n<details>\n<summary>large artifact</summary>\n\n" + strings.Repeat("artifact output\n", 300) + "</details>"
	got := fitValidationComment(
		validationCommentHeading,
		validationCommentHeadLabel+": `"+strings.Repeat("a", 40)+"`",
		"## Intent\n\n"+strings.Repeat("intent ", 200),
		strings.Repeat("risk ", 100),
		testing,
		pipelineMarkdownForTest(rounds...),
		4096,
	)
	if len(got) > 4096 {
		t.Fatalf("validation comment content = %d bytes, want <= 4096", len(got))
	}
	if !strings.Contains(got, strings.TrimSpace(validationTruncationMarker)) {
		t.Fatalf("truncated validation omitted its marker:\n%s", got)
	}
	if strings.Count(got, "<details>") != strings.Count(got, "</details>") {
		t.Fatalf("truncated validation left unbalanced details blocks:\n%s", got)
	}
	if !strings.Contains(got, "## Pipeline") {
		t.Fatalf("truncated validation lost the pipeline section without an omission marker:\n%s", got)
	}
}

func TestLegacyGeneratedDescriptionMigrationPreservesVisibleText(t *testing.T) {
	legacyText := "## What Changed\n\nHuman-adjusted summary.\n\n## Testing\n\nHuman note.\n\n"
	legacyMarker := pipelineAttestationCommentPrefix + `{"head_sha":"abc","steps":[]}` + pipelineAttestationCommentClosingToken
	legacy := legacyText + "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>✅ **Review** - passed</summary>\n\n✅ No issues found.\n\n</details>"
	parts, err := parseOrMigratePROwnedBody(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parts.before, "Human-adjusted summary.") || !strings.Contains(parts.before, "Human note.") || !strings.Contains(parts.before, "✅ No issues found.") || parts.managed {
		t.Fatalf("legacy migration changed visible text: %+v", parts)
	}
	appendix := joinBlocks(noMistakesPRSignature, legacyMarker)
	content, err := composeOwnedPRContent(parts, "", appendix, 0, scm.ProviderUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Body, "Human-adjusted summary.") || !strings.Contains(content.Body, "Human note.") || !strings.Contains(content.Body, "✅ No issues found.") || strings.Count(content.Body, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("legacy migration lost prose or duplicated attestation:\n%s", content.Body)
	}
}

func TestLegacyGeneratedDescriptionMigrationAcceptsFormerStepSummaries(t *testing.T) {
	legacyMarker := pipelineAttestationCommentPrefix + `{"head_sha":"abc","steps":[]}` + pipelineAttestationCommentClosingToken
	cases := []struct {
		summary string
		detail  string
	}{
		{"⏳ **CI** - pending", "Step has not started yet."},
		{"⏸️ **Review** - awaiting approval", "Waiting for user approval."},
		{"🔄 **Review** - auto-fixing", "Agent is currently applying fixes."},
		{"❌ **Test** - failed", "Step failed."},
		{"⏭️ **Document** - skipped", "Step was skipped."},
		{"⚠️ **Review** - findings unavailable", "No round details recorded."},
		{"⚠️ **Review** - medium risk", "✅ No issues found."},
		{"🚨 **Review** - high risk", "✅ No issues found."},
		{"✅ **Lint** - passed", "✅ No issues found."},
	}
	for _, tc := range cases {
		body := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>" + tc.summary + "</summary>\n\n" + tc.detail + "\n</details>"
		if _, err := parseOrMigratePROwnedBody(body); err != nil {
			t.Errorf("former summary %q was rejected: %v", tc.summary, err)
		}
	}
}

func TestLegacyMinimalDescriptionMigrationPreservesNarrativeAndRisk(t *testing.T) {
	legacyMarker := pipelineAttestationCommentPrefix + `{"head_sha":"abc","steps":[]}` + pipelineAttestationCommentClosingToken
	legacy := "Concise narrative.\n\n⚠️ Medium: publication changed\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker
	parts, err := parseOrMigratePROwnedBody(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(parts.before) != "Concise narrative.\n\n⚠️ Medium: publication changed" {
		t.Fatalf("minimal migration changed visible content: %q", parts.before)
	}
}

func TestLegacyGeneratedDescriptionMigrationRefusesQuotedOrNoncanonicalMarkers(t *testing.T) {
	legacyMarker := pipelineAttestationCommentPrefix + `{"head_sha":"abc","steps":[]}` + pipelineAttestationCommentClosingToken
	footer := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>✅ **Review** - passed</summary>\n\n✅ No issues found.\n\n</details>"
	quoted := "> " + strings.ReplaceAll(footer, "\n", "\n> ")
	minimal := noMistakesPRSignature + "\n\n" + legacyMarker
	cases := map[string]string{
		"code fence":                "Author archive:\n\n```markdown\n" + footer + "\n```",
		"blockquote":                "Author archive:\n\n" + quoted,
		"prose":                     "Author archive quotes " + noMistakesPRSignature + " beside " + legacyMarker,
		"nontrailing footer":        footer + "\n\n## Human notes\n\nKeep this marker as quoted history.",
		"plain suffix":              "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\nQuoted context.",
		"suffix after details":      footer + "\n\nQuoted context.",
		"malformed details":         "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>✅ **Review** - passed</summary>\n\nQuoted context.",
		"author archive details":    "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>Author archive</summary>\n\nQuoted context.\n</details>",
		"invented status details":   "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>✅ **Review** - archived</summary>\n\nQuoted context.\n</details>",
		"edited detail content":     "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>✅ **Review** - passed</summary>\n\nAuthor archive.\n</details>",
		"mismatched static detail":  "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>✅ **Review** - passed</summary>\n\nStep failed.\n</details>",
		"reordered severities":      "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>⚠️ **Review** - 2 issues (1 warning, 1 error)</summary>\n\nQuoted context.\n</details>",
		"wrong severity total":      "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>⚠️ **Review** - 3 issues (1 error, 1 warning)</summary>\n\nQuoted context.\n</details>",
		"duplicate fix outcome":     "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>🔧 **Review** - 2 issues found → auto-fixed → auto-fixed ✅</summary>\n\nQuoted context.\n</details>",
		"reordered fix outcomes":    "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>🔧 **Review** - 2 issues found → no changes applied → auto-fixed ✅</summary>\n\nQuoted context.\n</details>",
		"fix detail count mismatch": "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + legacyMarker + "\n\n<details>\n<summary>🔧 **Review** - 2 issues found → auto-fixed (2) ✅</summary>\n\n🔧 Fix applied.\n\n✅ Re-checked - no issues remain.\n</details>",
		"fenced minimal":            "```text\n" + minimal + "\n```",
		"embedded minimal":          "Author archive:\n\n" + minimal + "\n\nHuman suffix.",
		"duplicated minimal marker": minimal + "\n\n" + minimal,
	}
	_, appendix := ownedFixture(t)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			host := &ownershipRaceHost{body: body}
			err := updateOwnedPR(&pipeline.StepContext{Ctx: context.Background()}, host, &scm.PR{Number: "42"}, scm.PRContent{Title: "Author title", Body: body}, "", "", false, appendix, 0, scm.ProviderUnknown)
			if err == nil || host.writes != 0 || host.body != body {
				t.Fatalf("noncanonical legacy marker was claimed: err=%v writes=%d body=%q", err, host.writes, host.body)
			}
		})
	}
}
