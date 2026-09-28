package steps

import (
	"context"
	"encoding/json"
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
	creates         int
	updates         int
	listCalls       int
	hiddenListCalls map[int]bool
}

func (h *managedCommentTestHost) Provider() scm.Provider { return scm.ProviderGitHub }
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
	comment := scm.PRComment{ID: "7", Body: body}
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

func managedCommentContext(url string) *pipeline.StepContext {
	return &pipeline.StepContext{
		Ctx: context.Background(),
		Run: &db.Run{ID: "run-1", PRURL: &url},
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
	sctx := managedCommentContext(url)
	first := wrapValidationComment("first validation")
	if err := publishValidationComment(sctx, host, pr, first); err != nil {
		t.Fatal(err)
	}
	if err := publishValidationComment(sctx, host, pr, first); err != nil {
		t.Fatal(err)
	}
	second := wrapValidationComment("second validation")
	if err := publishValidationComment(sctx, host, pr, second); err != nil {
		t.Fatal(err)
	}
	if host.creates != 1 || host.updates != 1 || len(host.comments) != 2 {
		t.Fatalf("creates=%d updates=%d comments=%+v", host.creates, host.updates, host.comments)
	}
	if host.comments[0].Body != "maintainer note" || host.comments[1].ID != "7" || host.comments[1].Body != second {
		t.Fatalf("comment ownership was not preserved: %+v", host.comments)
	}
}

func TestPublishValidationCommentSettlesDelayedCreateVisibilityWithoutDuplicating(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	host := &managedCommentTestHost{hiddenListCalls: map[int]bool{2: true}}
	body := wrapValidationComment("validation")
	if err := publishValidationComment(managedCommentContext(url), host, &scm.PR{Number: "42", URL: url}, body); err != nil {
		t.Fatal(err)
	}
	if host.creates != 1 || host.updates != 0 || len(host.comments) != 1 {
		t.Fatalf("creates=%d updates=%d comments=%+v", host.creates, host.updates, host.comments)
	}
	if host.listCalls < 3 {
		t.Fatalf("list calls=%d, want a bounded settlement re-read", host.listCalls)
	}
}

func TestPublishValidationCommentRefusesDuplicateOrEditedOwnership(t *testing.T) {
	url := "https://github.com/test/repo/pull/42"
	pr := &scm.PR{Number: "42", URL: url}
	sctx := managedCommentContext(url)
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

func TestPublishValidationCommentRefusesSiblingReviewObject(t *testing.T) {
	ownedURL := "https://github.com/test/repo/pull/42"
	host := &managedCommentTestHost{}
	err := publishValidationComment(managedCommentContext(ownedURL), host, &scm.PR{Number: "43", URL: "https://github.com/test/repo/pull/43"}, wrapValidationComment("validation"))
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
	for _, want := range []string{validationCommentHeading, validationCommentHeadLabel + ": `" + head + "`", "## Intent", "## Risk Assessment", "## Testing", "## Pipeline", "go test ./...", "Provider contract tests passed"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("validation comment missing %q:\n%s", want, comment)
		}
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
	legacy := legacyText + noMistakesPRSignature + "\n\n" + legacyMarker
	parts, err := parseOrMigratePROwnedBody(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parts.before, "Human-adjusted summary.") || !strings.Contains(parts.before, "Human note.") || parts.managed {
		t.Fatalf("legacy migration changed visible text: %+v", parts)
	}
	appendix := joinBlocks(noMistakesPRSignature, legacyMarker)
	content, err := composeOwnedPRContent(parts, "", appendix, 0, scm.ProviderUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Body, "Human-adjusted summary.") || !strings.Contains(content.Body, "Human note.") || strings.Count(content.Body, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("legacy migration lost prose or duplicated attestation:\n%s", content.Body)
	}
}
