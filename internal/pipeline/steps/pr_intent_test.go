package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPROmitIntentSuppressionIsTightenOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		runOmitIntent  bool
		repoPolicy     string
		wantIntSection bool
	}{
		// Without the stamp the repository's trusted policy alone decides.
		{name: "default publishes", runOmitIntent: false, repoPolicy: "pr: {}", wantIntSection: true},
		// The run stamp removes the section even where the repo publishes.
		{name: "run stamp removes", runOmitIntent: true, repoPolicy: "pr: {}", wantIntSection: false},
		{name: "run stamp removes with repo publish", runOmitIntent: true, repoPolicy: "pr: {publish_intent: true}", wantIntSection: false},
		// The repository's trusted ceiling is untouched by anything else.
		{name: "trusted ceiling wins", runOmitIntent: false, repoPolicy: "pr: {publish_intent: false}", wantIntSection: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"title":"feat: helper","body":"## What Changed\n\n- A helper."}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Run.OmitIntent = tc.runOmitIntent
			trusted, err := config.LoadRepoFromBytes([]byte(tc.repoPolicy))
			if err != nil {
				t.Fatal(err)
			}
			sctx.Config.PR = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(nil, trusted, false)).PR
			sctx.UserIntent = "Entire original intent remains reviewer input."
			step := &PRStep{}
			got, err := step.buildPRContent(sctx, "feature", "main", base, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			comment, err := step.renderValidationComment(sctx, scm.ProviderGitHub)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(comment, "## Intent") != tc.wantIntSection {
				t.Fatalf("runOmit=%v policy=%s: comment has Intent section = %v, want %v:\n%s", tc.runOmitIntent, tc.repoPolicy, !tc.wantIntSection, tc.wantIntSection, comment)
			}
			if strings.Contains(got.Body, "## Intent") || strings.Contains(got.Body, "## What Changed") || !strings.Contains(got.Body, "A helper") {
				t.Fatalf("description was not concise:\n%s", got.Body)
			}
			// The repository policy alone never withholds the intent from the
			// drafter; the caller-side omission does (see the test below).
			if got := strings.Contains(ag.calls[len(ag.calls)-1].Prompt, sctx.UserIntent); got == tc.runOmitIntent {
				t.Fatalf("drafting prompt contains intent = %v under runOmit=%v", got, tc.runOmitIntent)
			}
		})
	}
}

// Under the caller-side omission the PR-drafting turns receive no intent text
// at all, on both the ordinary narrative path and the repository-template
// path: withhold, never scan. Every other step prompt keeps the full intent.
func TestPROmitIntentWithholdsIntentFromDraftingTurns(t *testing.T) {
	t.Parallel()
	const secret = "Private goal: replace the vendor before the contract renews."
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (*pipeline.StepContext, *mockAgent)
	}{
		{name: "ordinary narrative", setup: func(t *testing.T) (*pipeline.StepContext, *mockAgent) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"title":"feat: helper","body":"## What Changed\n\n- A helper."}`)}, nil
			}}
			return newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{}), ag
		}},
		{name: "repository template narrative", setup: func(t *testing.T) (*pipeline.StepContext, *mockAgent) {
			sctx, ag, _ := templateTestContext(t)
			return sctx, ag
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, ag := tc.setup(t)
			sctx.UserIntent = secret
			sctx.Run.OmitIntent = true
			step := &PRStep{}
			got, err := step.buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			comment, err := step.renderValidationComment(sctx, scm.ProviderGitHub)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) == 0 {
				t.Fatal("no drafting turn ran")
			}
			for i, call := range ag.calls {
				if strings.Contains(call.Prompt, secret) || strings.Contains(call.Prompt, "USER INTENT") {
					t.Fatalf("drafting turn %d received intent text:\n%s", i, call.Prompt)
				}
			}
			if strings.Contains(got.Body+comment, secret) || strings.Contains(got.Body+comment, "## Intent") {
				t.Fatalf("publication exposed withheld intent:\n%s\n%s", got.Body, comment)
			}
			// Other step prompts are unchanged: the full intent still reaches them.
			if !strings.Contains(userIntentPromptSection(sctx), secret) {
				t.Fatal("omission leaked into the shared step prompt section")
			}
		})
	}
}

func TestPRPublishIntentSuppressionCoversDefaultAgentAndFallback(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	for _, tc := range []struct {
		name    string
		publish *bool
		want    bool
	}{
		{"default", nil, true},
		{"enabled", &yes, true},
		{"disabled", &no, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, fallback := range []bool{false, true} {
				dir, base, head := setupGitRepo(t)
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					if fallback {
						return nil, errors.New("draft unavailable")
					}
					return &agent.Result{Output: json.RawMessage(`{"title":"feat: helper","body":"## What Changed\n\n- A helper."}`)}, nil
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				policy := "pr: {}"
				if tc.publish != nil {
					policy = fmt.Sprintf("pr: {publish_intent: %t}", *tc.publish)
				}
				trusted, err := config.LoadRepoFromBytes([]byte(policy))
				if err != nil {
					t.Fatal(err)
				}
				pushed, err := config.LoadRepoFromBytes([]byte(fmt.Sprintf("pr: {publish_intent: %t}", !tc.want)))
				if err != nil {
					t.Fatal(err)
				}
				sctx.Config.PR = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(pushed, trusted, true)).PR
				sctx.UserIntent = "Entire original intent remains reviewer input."
				for _, limit := range []int{0, 4000} {
					step := &PRStep{}
					got, err := step.buildPRContent(sctx, "feature", "main", base, scm.ProviderGitHub, limit)
					if err != nil {
						t.Fatalf("fallback=%v limit=%d: %+v, %v", fallback, limit, got, err)
					}
					comment, err := step.renderValidationComment(sctx, scm.ProviderGitHub)
					if err != nil || strings.Contains(comment, "## Intent") != tc.want || strings.Contains(got.Body, "## Intent") {
						t.Fatalf("fallback=%v limit=%d description=%q comment=%q err=%v", fallback, limit, got.Body, comment, err)
					}
					t.Logf("Generated PR publication: policy=%s fallback=%v limit=%d\n%s\n%s", tc.name, fallback, limit, got.Body, comment)
					if tc.want && !strings.Contains(comment, "## Intent\n\n"+sctx.UserIntent) {
						t.Fatalf("default/enabled publication lost intent: %s", comment)
					}
					if !strings.Contains(ag.calls[len(ag.calls)-1].Prompt, sctx.UserIntent) {
						t.Fatal("suppression removed full intent from model context")
					}
				}
			}
		})
	}
}
