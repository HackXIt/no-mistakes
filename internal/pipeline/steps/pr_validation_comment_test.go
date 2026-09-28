package steps

import (
	"context"
	"encoding/json"
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
	comments []scm.PRComment
	creates  int
	updates  int
}

func (h *managedCommentTestHost) Provider() scm.Provider { return scm.ProviderGitHub }
func (h *managedCommentTestHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{ManagedPRComments: true}
}
func (h *managedCommentTestHost) ListPRComments(context.Context, *scm.PR) ([]scm.PRComment, error) {
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
	for _, want := range []string{validationCommentHeading, "## Intent", "## Risk Assessment", "## Testing", "## Pipeline", "go test ./...", "Provider contract tests passed"} {
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
	content, err := composeOwnedPRContent(parts, "", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Body, "Human-adjusted summary.") || !strings.Contains(content.Body, "Human note.") || strings.Count(content.Body, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("legacy migration lost prose or duplicated attestation:\n%s", content.Body)
	}
}
