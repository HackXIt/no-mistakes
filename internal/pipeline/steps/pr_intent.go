package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// publicPRIntent returns the intent text published in the managed validation
// comment, or "" when publication is suppressed. It is the single publication
// gate, so caller-side omission and the repository's trusted policy compose
// here and nowhere else.
//
// The control is tighten-only in both directions:
//
//   - The repository's trusted pr.publish_intent (default true) is the
//     ceiling. A caller can never publish intent on a repository whose
//     trusted config disabled it.
//   - The caller-side decision recorded on the run (runs.omit_intent: the
//     axi run --no-publish-intent flag, or the operator's global
//     intent.publish_intent: false default, folded together at run start)
//     can only remove the section, never restore it.
//
// The repository policy never touches the intent that reaches step prompts.
// The caller-side omission is different: it also WITHHOLDS the intent from the
// PR-drafting turns (prDraftIntentPromptSection), because a PR title or body
// drafted with the intent in context can paraphrase it into the public text,
// and there is deliberately no output filter or prose scanner to catch that.
// Review, test, document, lint, and CI-fix prompts keep the full intent
// under either signal.
func publicPRIntent(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil && !sctx.Config.PR.PublishesIntent() {
		return ""
	}
	if runOmitsIntent(sctx) {
		return ""
	}
	return cleanedUserIntent(sctx)
}

// runOmitsIntent reports whether this run was started with the caller-side,
// tighten-only request to keep the generated Intent section out of the managed
// validation comment. The decision is stamped on the run row at start, so it
// survives daemon restarts and reruns.
func runOmitsIntent(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Run != nil && sctx.Run.OmitIntent
}

// prDraftIntentPromptSection is the intent section for the PR-drafting turns
// (ordinary narrative, title-only fallback, and repository-template
// narrative). Under caller-side omission it is empty, so those turns cannot
// paraphrase intent into the generated title or description. Every other step
// prompt keeps userIntentPromptSection unchanged, and its recorded output may
// still reach detailed managed validation.
func prDraftIntentPromptSection(sctx *pipeline.StepContext) string {
	if runOmitsIntent(sctx) {
		return ""
	}
	return userIntentPromptSection(sctx)
}
