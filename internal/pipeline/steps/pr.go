package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/conventional"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PRStep creates or updates a pull request via the provider CLI or API.
type PRStep struct {
	// mediaUploader uploads image/video evidence at PR render time. Nil uses
	// the GitHub host's user-attachments client. Tests inject a stub so they
	// never talk to live GitHub.
	mediaUploader userAssetUploader
}

type prContent struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

var prContentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string", "description": "Concise pull request title following repository configuration"},
		"body": {"type": "string", "description": "Concise squash-commit-style GitHub-flavored markdown description with no title or validation sections. Plain text, NOT JSON."}
	},
	"required": ["title", "body"]
}`)

var prTitleSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string", "description": "Bare concise pull request title text"}
	},
	"required": ["title"]
}`)

const (
	githubPullRequestBodyHardLimitChars = 65536
	// Count bytes, not runes, so multi-byte markdown still stays under
	// GitHub's character limit with room for provider-side formatting drift.
	pullRequestBodySafetyBufferBytes = 2048
	maxPullRequestBodyBytes          = githubPullRequestBodyHardLimitChars - pullRequestBodySafetyBufferBytes
	minLatestPipelineUpdateBytes     = 256
	maxSquashDescriptionBytes        = 1200
	maxSquashDescriptionBulletBytes  = 400
)

type pipelineUpdateGroup struct {
	header string
	units  []string
	footer string
}

func (s *PRStep) Name() types.StepName { return types.StepPR }

func (s *PRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx

	branch := sctx.Run.Branch
	if strings.HasPrefix(branch, "refs/heads/") {
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}
	baseBranch := effectivePRBaseBranch(sctx)
	if branch == baseBranch {
		sctx.Log(fmt.Sprintf("skipping PR creation on base branch %s", branch))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	provider := resolvedProvider(sctx)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log(fmt.Sprintf("skipping PR creation: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: skipReason}, nil
	}
	if err := host.Available(ctx); err != nil {
		sctx.Log(fmt.Sprintf("skipping PR creation: %v", err))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: err.Error()}, nil
	}

	// Capture live author content before model drafting. An unreadable
	// provider cannot promise template preservation.
	var template string
	if name := configuredPRTemplate(sctx); name != "" {
		if _, ok := host.(scm.PRContentReader); !ok {
			return nil, fmt.Errorf("pr.template requires raw PR content reads; this provider is unsupported")
		}
		var err error
		template, err = loadPRTemplate(ctx, sctx.WorkDir, sctx.Config.TrustedConfigSHA, name)
		if err != nil {
			return nil, err
		}
	}
	baseSHA, err := resolveBranchBaseSHA(ctx, sctx, sctx.Run.BaseSHA, baseBranch)
	if err != nil {
		return nil, err
	}
	bodyLimit := scm.MaxPRBodyChars(provider)
	sctx.Log(fmt.Sprintf("checking for existing pull request on branch %s...", branch))
	existing, err := host.FindPR(ctx, branch, "")
	if err != nil {
		return nil, err
	}
	existing, replaceStaleIdentity, err := bindExistingPR(sctx, host, existing)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		reader, ok := host.(scm.PRContentReader)
		if !ok {
			return nil, fmt.Errorf("provider cannot read existing PR content for author-safe publication")
		}
		live, err := reader.GetPRContent(ctx, existing)
		if err != nil {
			return nil, fmt.Errorf("read existing PR before publication: %w", err)
		}
		if err := persistRunPRURL(sctx, existing.URL, replaceStaleIdentity); err != nil {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("pull request already exists: %s, updating...", describePR(existing)))

		var emptyNarrative string
		var title string
		if live.Body == "" {
			if template != "" {
				draft, err := s.draftTemplateNarrative(sctx, branch, baseBranch, baseSHA, template)
				if err != nil {
					return nil, err
				}
				emptyNarrative = neutralizeAttestationMarkers(draft.Body)
				title = draft.Title
			} else {
				draft, err := s.draftPRContent(sctx, branch, baseBranch, baseSHA, provider, bodyLimit)
				if err != nil {
					return nil, err
				}
				emptyNarrative = neutralizeAttestationMarkers(draft.Body)
				title = draft.Title
			}
		} else if sctx.Config != nil && sctx.Config.PR.TitleFormat != "" {
			title, err = s.draftConfiguredPRTitle(sctx, branch, baseBranch, baseSHA)
			if err != nil {
				return nil, err
			}
		}
		appendix, err := s.buildPRAppendix(sctx, provider)
		if err != nil {
			return nil, err
		}
		if err := retargetExistingPRIfNeeded(sctx, host, existing, runPRBaseBranch(sctx)); err != nil {
			return nil, err
		}
		if err := updateOwnedPR(sctx, host, existing, live, title, emptyNarrative, appendix, bodyLimit); err != nil {
			return nil, err
		}
		comment, err := s.renderValidationComment(sctx, provider)
		if err != nil {
			return nil, err
		}
		if err := publishValidationComment(sctx, host, existing, comment); err != nil {
			return nil, err
		}
		if existing.URL != "" {
			return &pipeline.StepOutcome{PRURL: existing.URL}, nil
		}
		return &pipeline.StepOutcome{}, nil
	}

	content, err := s.buildPRContent(sctx, branch, baseBranch, baseSHA, provider, bodyLimit)
	if err != nil {
		return nil, err
	}
	sctx.Log("creating pull request...")
	created, err := host.CreatePR(ctx, branch, baseBranch, scm.PRContent(content))
	if err != nil {
		return nil, err
	}
	if created == nil || strings.TrimSpace(created.URL) == "" {
		return nil, fmt.Errorf("PR create returned no readable review-object identity")
	}
	sctx.Log(fmt.Sprintf("created pull request: %s", created.URL))
	if err := persistRunPRURL(sctx, created.URL, replaceStaleIdentity); err != nil {
		return nil, err
	}
	if template != "" {
		reader, ok := host.(scm.PRContentReader)
		if !ok {
			return nil, fmt.Errorf("provider cannot verify the created template description")
		}
		actual, err := reader.GetPRContent(ctx, created)
		if err != nil {
			return nil, fmt.Errorf("verify created template description: %w", err)
		}
		if actual.Body != content.Body {
			return nil, fmt.Errorf("created PR description differs from the proposed template and compact attestation; refusing successful publication")
		}
	}
	comment, err := s.renderValidationComment(sctx, provider)
	if err != nil {
		return nil, err
	}
	if err := publishValidationComment(sctx, host, created, comment); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{PRURL: created.URL}, nil
}

// retargetExistingPRIfNeeded moves an already-open PR onto a per-run
// --base-branch override when the live forge base disagrees. Repo-config
// pr.base_branch changes still do not retarget: requested is empty in that
// path, so title and body update in place and CI keeps following the live
// forge base.
func retargetExistingPRIfNeeded(sctx *pipeline.StepContext, host scm.Host, existing *scm.PR, requested string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" || existing == nil {
		return nil
	}
	actual := strings.TrimSpace(existing.BaseBranch)
	if actual == requested {
		return nil
	}
	if err := requireOwnedPRIdentity(sctx, existing); err != nil {
		return err
	}
	retargeter, ok := host.(scm.PRBaseRetargeter)
	if !ok {
		if actual == "" {
			return fmt.Errorf("existing pull request %s has no readable base branch, and this provider cannot retarget it to %s", describePR(existing), requested)
		}
		return fmt.Errorf("existing pull request %s targets %s, not %s, and this provider cannot retarget it", describePR(existing), actual, requested)
	}
	from := actual
	if from == "" {
		from = "its current base"
	}
	sctx.Log(fmt.Sprintf("retargeting existing pull request %s from %s to %s", describePR(existing), from, requested))
	if err := retargeter.SetPRBaseBranch(sctx.Ctx, existing, requested); err != nil {
		return fmt.Errorf("retarget pull request to %s: %w", requested, err)
	}
	existing.BaseBranch = requested
	return nil
}

// bindExistingPR prefers the run's persisted PR URL over a branch-only
// FindPR hit after GetPRState proves that identity is still open. A closed
// or merged persisted PR is stale: title/body update the discovered PR, and
// a per-run --base-branch retarget is refused rather than moving either
// object. First-attach (no persisted URL) keeps the discovered PR.
func bindExistingPR(sctx *pipeline.StepContext, host scm.Host, discovered *scm.PR) (*scm.PR, bool, error) {
	owned := runPRURL(sctx)
	if owned == "" {
		return discovered, false, nil
	}
	if host == nil {
		return nil, false, fmt.Errorf("read persisted pull request %s state: host unavailable", owned)
	}
	ownedPR := discovered
	if !samePRIdentity(owned, discovered) {
		ownedPR = prFromOwnedURL(owned)
	}
	ctx := context.Background()
	if sctx != nil && sctx.Ctx != nil {
		ctx = sctx.Ctx
	}
	state, err := host.GetPRState(ctx, ownedPR)
	if err != nil {
		return nil, false, fmt.Errorf("read persisted pull request %s state: %w", owned, err)
	}
	if state != scm.PRStateOpen {
		if runPRBaseBranch(sctx) != "" {
			return nil, false, fmt.Errorf("persisted pull request %s is stale (%s); refusing to retarget another pull request", owned, strings.ToLower(string(state)))
		}
		return discovered, true, nil
	}
	existing := discovered
	if !samePRIdentity(owned, discovered) {
		existing = ownedPR
		if sctx != nil && sctx.Log != nil {
			sctx.Log(fmt.Sprintf("using persisted pull request %s instead of discovered %s", owned, describePR(discovered)))
		}
	}
	if strings.TrimSpace(existing.BaseBranch) != "" {
		return existing, false, nil
	}
	reader, ok := host.(scm.PRBaseBranchReader)
	if !ok {
		return existing, false, nil
	}
	base, err := reader.GetPRBaseBranch(ctx, existing)
	if err != nil {
		return nil, false, fmt.Errorf("read persisted pull request %s: %w", owned, err)
	}
	existing.BaseBranch = strings.TrimSpace(base)
	return existing, false, nil
}

func prFromOwnedURL(owned string) *scm.PR {
	pr := &scm.PR{URL: owned}
	if n, err := scm.ExtractPRNumber(owned); err == nil {
		pr.Number = n
	}
	return pr
}

// requireOwnedPRIdentity fails closed unless the PR about to be mutated is
// proven to be the run's persisted review object. Retarget uses this before
// any base move. Title/body update of a first-attach FindPR hit (no
// persisted URL) still proceeds so a later pr.base_branch change updates
// the open PR instead of opening a duplicate.
func requireOwnedPRIdentity(sctx *pipeline.StepContext, existing *scm.PR) error {
	owned := runPRURL(sctx)
	if owned == "" {
		return fmt.Errorf("refusing to retarget pull request %s: this run has no persisted PR identity", describePR(existing))
	}
	if samePRIdentity(owned, existing) {
		return nil
	}
	return fmt.Errorf("discovered pull request %s does not match this run's persisted pull request %s", describePR(existing), owned)
}

func runPRURL(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.PRURL == nil {
		return ""
	}
	return strings.TrimSpace(*sctx.Run.PRURL)
}

// persistRunPRURL establishes the sole review-object identity before either
// publication surface is mutated. The managed comment must never be selected
// from a branch lookup while the durable run still names a sibling PR.
func persistRunPRURL(sctx *pipeline.StepContext, raw string, replaceStaleIdentity bool) error {
	raw = strings.TrimSpace(raw)
	if sctx == nil || sctx.Run == nil || raw == "" {
		return fmt.Errorf("cannot persist an empty pull request identity")
	}
	if owned := runPRURL(sctx); owned != "" && !strings.EqualFold(strings.TrimRight(owned, "/"), strings.TrimRight(raw, "/")) && !replaceStaleIdentity {
		return fmt.Errorf("refusing to replace persisted pull request %s with sibling %s", owned, raw)
	}
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, raw); err != nil {
		return fmt.Errorf("persist pull request identity: %w", err)
	}
	value := raw
	sctx.Run.PRURL = &value
	return nil
}

func samePRIdentity(ownedURL string, discovered *scm.PR) bool {
	ownedURL = strings.TrimRight(strings.TrimSpace(ownedURL), "/")
	if ownedURL == "" || discovered == nil {
		return false
	}
	discoveredURL := strings.TrimRight(strings.TrimSpace(discovered.URL), "/")
	if discoveredURL != "" {
		return strings.EqualFold(ownedURL, discoveredURL)
	}
	ownedNum, err := scm.ExtractPRNumber(ownedURL)
	if err != nil || ownedNum == "" {
		return false
	}
	return discovered.Number != "" && discovered.Number == ownedNum
}

func describePR(pr *scm.PR) string {
	if pr == nil {
		return ""
	}
	if pr.URL != "" {
		return pr.URL
	}
	if pr.Number != "" {
		return "#" + pr.Number
	}
	return ""
}

// buildPRContent drafts the pull request title and body and then applies the
// publication redaction boundary. New template bodies and author-preserving
// updates use composeOwnedPRContent, which calls the same redactPRContent owner
// before stamping its integrity guard. This covers every source: agent-authored
// prose, extracted user intent, findings, fix summaries, step errors, artifact
// paths, artifact captions, and captured output embedded from evidence files.
//
// The scrub deliberately sits here rather than at each of those sources. A
// per-source scrub is a set of guards that has to be complete to work, and the
// next rendering path somebody adds is not going to have one; a boundary scrub
// covers sources nobody has written yet.
func (s *PRStep) buildPRContent(sctx *pipeline.StepContext, branch, baseBranch, baseSHA string, provider scm.Provider, bodyLimit int) (prContent, error) {
	if name := configuredPRTemplate(sctx); name != "" {
		if !supportsPRTemplates(provider) {
			return prContent{}, fmt.Errorf("pr.template is unsupported by this provider")
		}
		template, err := loadPRTemplate(sctx.Ctx, sctx.WorkDir, sctx.Config.TrustedConfigSHA, name)
		if err != nil {
			return prContent{}, err
		}
		content, err := s.draftTemplateNarrative(sctx, branch, baseBranch, baseSHA, template)
		if err != nil {
			return prContent{}, err
		}
		appendix, err := s.buildPRAppendix(sctx, provider)
		if err != nil {
			return prContent{}, err
		}
		return composeOwnedPRContent(prOwnedBody{before: neutralizeAttestationMarkers(content.Body)}, content.Title, appendix, bodyLimit)
	}
	content, err := s.draftPRContent(sctx, branch, baseBranch, baseSHA, provider, bodyLimit)
	if err != nil {
		return prContent{}, err
	}
	appendix, err := s.buildPRAppendix(sctx, provider)
	if err != nil {
		return prContent{}, err
	}
	narrative := neutralizeAttestationMarkers(content.Body)
	if bodyLimit > 0 {
		overhead := scm.PRBodyLen("\n\n" + wrapPRAppendix(appendix))
		if available := bodyLimit - overhead; available > 0 && scm.PRBodyLen(narrative) > available {
			narrative = scm.ClampPRBody(narrative, available)
		}
	}
	return composeOwnedPRContent(prOwnedBody{before: narrative}, content.Title, appendix, bodyLimit)
}

// redactPRContent removes the operator's home directory from the content about
// to be published. Ordinary drafts call it after length caps; the placeholder
// never grows a path. Owned composition calls it before its integrity guard
// and non-truncating size check so publication cannot invalidate that guard.
func redactPRContent(content prContent) prContent {
	content.Title = safepath.RedactText(content.Title)
	content.Body = safepath.RedactText(content.Body)
	return content
}

func (s *PRStep) draftPRContent(sctx *pipeline.StepContext, branch, baseBranch, baseSHA string, provider scm.Provider, bodyLimit int) (prContent, error) {
	ctx := sctx.Ctx
	diffStat, _ := git.Run(ctx, sctx.WorkDir, "diff", "--stat", baseSHA+".."+sctx.Run.HeadSHA)
	finalDiff, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-status", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return prContent{}, fmt.Errorf("read final branch diff: %w", err)
	}
	titleRules := prTitlePromptRules(sctx)
	scopeRules := prTitleScopeRules(sctx)
	prompt := fmt.Sprintf(`Draft a pull request title and summary for the full branch delta.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR base branch: %s

Rules:
- Cover the full branch delta, not just the latest commit.
%s
%s
- Body: a concise squash-commit-style description in GitHub-flavored markdown: one short paragraph or 1-3 brief bullets describing the concrete branch delta. Do not repeat the title, enumerate commits, add a What Changed heading, or include Intent, Risk Assessment, Testing, Pipeline, command output, scenario tables, chronology, or validation logs. The body value must be plain markdown text, never a JSON object or serialized JSON string.
- Derive every body claim from the final diff. Inspect it directly when the paths and statuses below do not provide enough detail.
- Do not invent tests or behavior.

Diff stat:
%s

Final diff paths and statuses:
%s%s%s`, branch, baseSHA, sctx.Run.HeadSHA, baseBranch, titleRules, scopeRules, diffStat, finalDiff, prDraftIntentPromptSection(sctx), executionContextPromptSection(sctx.WorkDir))

	prompt += prBodyBudgetPromptSection(bodyLimit)
	prompt += agent.MemoryFilesRule

	result, err := sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prContentSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		slog.Warn("agent failed for PR content, using fallback", "error", err)
		fallback, fallbackErr := fallbackPRContent(sctx, finalDiff, bodyLimit)
		return fallback, fallbackErr
	}

	var content prContent
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &content); err == nil {
			content.Title = strings.TrimSpace(content.Title)
			content.Body = strings.TrimSpace(content.Body)
			content.Body = unwrapNestedPRBody(content.Body)
			content.Body = stripGeneratedSections(content.Body)
			if content.Title != "" {
				originalTitle := content.Title
				content.Title, err = renderPRTitle(sctx, content.Title)
				if err != nil {
					return prContent{}, err
				}
				if content.Title != originalTitle {
					slog.Warn("normalized agent PR title", "from", originalTitle, "to", content.Title)
				}
				normalizedBody, valid := normalizeSquashDescription(content.Body, content.Title)
				content.Body = neutralizeAttestationMarkers(normalizedBody)
				if !valid {
					return fallbackPRContent(sctx, finalDiff, bodyLimit)
				}
				// Detailed recorded validation is rendered into the managed PR
				// comment. Keep the description suitable for a squash commit body.
				if bodyLimit > 0 && scm.PRBodyLen(content.Body) > bodyLimit {
					content.Body = scm.ClampPRBody(content.Body, bodyLimit)
				}
				return content, nil
			}
		}
	}

	return fallbackPRContent(sctx, finalDiff, bodyLimit)
}

func (s *PRStep) draftConfiguredPRTitle(sctx *pipeline.StepContext, branch, baseBranch, baseSHA string) (string, error) {
	paths, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-status", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("read final branch diff for PR title: %w", err)
	}
	prompt := fmt.Sprintf(`Draft only the bare concise pull request title text for the full final branch delta.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR base branch: %s

Rules:
- Return only the title component in the structured title field.
- Do not include a branch identifier or any repository formatter prefix or suffix; those are applied deterministically after drafting.
- Derive the title from the final diff and inspect it directly when the paths below do not provide enough detail.
- Do not invent behavior.

Final diff paths and statuses:
%s%s%s`, branch, baseSHA, sctx.Run.HeadSHA, baseBranch, paths, prDraftIntentPromptSection(sctx), executionContextPromptSection(sctx.WorkDir))
	prompt += agent.MemoryFilesRule
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prTitleSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return "", fmt.Errorf("draft configured PR title: %w", err)
	}
	var content prContent
	if result == nil || json.Unmarshal(result.Output, &content) != nil || strings.TrimSpace(content.Title) == "" {
		return "", fmt.Errorf("agent returned no valid configured PR title")
	}
	title, err := renderPRTitle(sctx, strings.TrimSpace(content.Title))
	if err != nil {
		return "", err
	}
	return title, nil
}

// normalizeSquashDescription enforces the default description's public shape
// instead of trusting prompt compliance. A repository template has its own
// structural contract and never passes through here. Invalid model output uses
// the deterministic concise fallback rather than publishing logs or a second
// long-form narrative surface.
func normalizeSquashDescription(body, title string) (string, bool) {
	body = strings.TrimSpace(body)
	lines := strings.Split(body, "\n")
	if len(lines) == 0 {
		return "", false
	}
	first := strings.ToLower(strings.TrimSpace(strings.TrimLeft(lines[0], "#")))
	if first == "summary" || first == "what changed" || first == "description" || first == "overview" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return "", false
	}

	var nonempty []string
	hasInteriorBlank := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			hasInteriorBlank = true
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") || strings.HasPrefix(line, "|") {
			return "", false
		}
		nonempty = append(nonempty, line)
	}
	if len(nonempty) == 0 {
		return "", false
	}

	allBullets := true
	for _, line := range nonempty {
		if !(strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ")) {
			allBullets = false
			break
		}
	}
	if allBullets {
		if len(nonempty) > 3 {
			return "", false
		}
		bullets := make([]string, 0, len(nonempty))
		for _, line := range nonempty {
			text := strings.TrimSpace(line[2:])
			if text == "" || len(text) > maxSquashDescriptionBulletBytes || repeatsPRTitle(text, title) {
				return "", false
			}
			bullets = append(bullets, "- "+text)
		}
		normalized := strings.Join(bullets, "\n")
		return normalized, len(normalized) <= maxSquashDescriptionBytes
	}

	if hasInteriorBlank {
		return "", false
	}
	for _, line := range nonempty {
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "+ ") || numberedListLine(line) {
			return "", false
		}
	}
	paragraph := strings.Join(nonempty, " ")
	if repeatsPRTitle(paragraph, title) || len(paragraph) > maxSquashDescriptionBytes {
		return "", false
	}
	return paragraph, true
}

func numberedListLine(line string) bool {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' '
}

func repeatsPRTitle(body, title string) bool {
	normalize := func(value string) string {
		value = strings.TrimSpace(value)
		value = strings.TrimSuffix(value, ".")
		return strings.Join(strings.Fields(value), " ")
	}
	return title != "" && strings.EqualFold(normalize(body), normalize(title))
}

func prTitlePromptRules(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil && sctx.Config.PR.TitleFormat != "" {
		return "- Title must be only the bare concise title text used by the repository's configured title formatter. Do not include a branch identifier or any formatter prefix or suffix; those are applied deterministically after drafting."
	}
	return "- Title must use conventional commit format: \"type(scope): description\" or \"type: description\". Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert. Scope is optional. Do not capitalize the type. Do not use the raw branch name.\n" + conventional.ReleaseTypeRule
}

func prTitleScopeRules(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil && sctx.Config.PR.TitleFormat != "" {
		return ""
	}
	return "- When including a scope, it MUST be a real package/module name that exists in the codebase (for example, a directory under internal/, cmd/, or the equivalent top-level grouping for this project), identified by inspecting the changed paths. Pick the primary module affected by the change, not a secondary or incidental one.\n- Keep the scope at a coarse level, not too granular: a codebase typically has fewer than 10 distinct scopes in use across its history. Prefer a broad module name (e.g. \"daemon\", \"pipeline\", \"cli\") over a narrow file or sub-feature name. If you cannot confidently identify a real primary module, omit the scope and use \"type: description\"."
}

func renderPRTitle(sctx *pipeline.StepContext, title string) (string, error) {
	if sctx == nil || sctx.Config == nil || sctx.Config.PR.TitleFormat == "" {
		return conventional.TightenTitle(title), nil
	}
	branch := strings.TrimSpace(strings.TrimPrefix(sctx.Run.Branch, "refs/heads/"))
	if sctx.Config.PR.RequiresBranch() {
		var err error
		branch, err = sctx.Config.Commit.BranchValue(sctx.Run.Branch)
		if err != nil {
			return "", fmt.Errorf("resolve branch identifier for PR title: %w", err)
		}
	}
	return sctx.Config.PR.RenderTitle(branch, title)
}

func loadPRPipelineRecords(sctx *pipeline.StepContext) ([]*db.StepResult, map[string][]*db.StepRound, error) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("query step results for PR publication: %w", err)
	}
	rounds := make(map[string][]*db.StepRound, len(steps))
	for _, sr := range steps {
		r, err := sctx.DB.GetRoundsByStep(sr.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("query %s rounds for PR publication: %w", sr.StepName, err)
		}
		rounds[sr.ID] = r
	}
	return steps, rounds, nil
}

func (s *PRStep) buildPipelineSectionFromRecords(sctx *pipeline.StepContext, provider scm.Provider, steps []*db.StepResult, rounds map[string][]*db.StepRound) (pipelineMD, riskLine, testingMD string) {
	policy := pipelineAttestationPolicy{}
	if sctx.Config != nil {
		policy.AllowTestCommandOverride = strings.TrimSpace(sctx.Config.Test.AllowApproveOverFailure)
	}
	pipelineMD, riskLine = buildPipelineSummaryFor(steps, rounds, sctx.Run.HeadSHA, provider, policy)
	if appendixMode(sctx) == config.PRAppendixMinimal {
		return "", riskLine, ""
	}
	// The review conversation rides inside the Pipeline section as an ordinary
	// `### ` group, so the comment-budget logic can drop it as a unit.
	if conversationMD := buildReviewConversationSection(sctx); conversationMD != "" && pipelineMD != "" {
		pipelineMD += "\n\n" + conversationMD
	}
	testingMD = buildPRTestingSummary(steps, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir, testEvidenceDir(sctx), publishRunEvidence(sctx), provider, s.attachRunEvidenceMedia(sctx, provider, steps, rounds))
	return pipelineMD, riskLine, testingMD
}

// unwrapNestedPRBody detects when the agent returned the body as a
// serialized prContent JSON string and extracts the real markdown body.
func unwrapNestedPRBody(body string) string {
	if len(body) == 0 || body[0] != '{' {
		return body
	}
	var nested prContent
	if err := json.Unmarshal([]byte(body), &nested); err != nil {
		return body
	}
	if strings.TrimSpace(nested.Body) != "" {
		slog.Warn("agent returned nested JSON in PR body, unwrapping")
		return strings.TrimSpace(nested.Body)
	}
	return body
}

// appendGeneratedSections appends deterministic sections after the agent's body
// and applies the PR body length guard.
// prBodyBudgetPromptSection tells the drafting agent about a host's PR-body
// character cap so it keeps its "## What Changed" section short. The Intent,
// Risk, Testing, and Pipeline sections are appended deterministically, so the
// agent only controls a slice of the budget; this nudge keeps that slice small.
// Returns "" when the provider has no practical limit (bodyLimit <= 0).
func prBodyBudgetPromptSection(bodyLimit int) string {
	if bodyLimit <= 0 {
		return ""
	}
	return fmt.Sprintf("\n\n- This repository's host caps the entire PR description at %d characters. Code appends a compact machine trailer automatically. Keep the squash-commit-style body to one short paragraph or at most three brief bullets.", bodyLimit)
}

func truncatePipelineSection(pipelineMD string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(pipelineMD) <= maxBytes {
		return pipelineMD
	}

	header, updates := splitPipelineSectionHeader(pipelineMD)
	groups := parsePipelineUpdateGroups(updates)
	totalUnits := countPipelineUpdateUnits(groups)
	if totalUnits == 0 {
		return pipelineOmissionSectionWithinLimit(header, 0, maxBytes)
	}

	for omitted := 1; omitted < totalUnits; omitted++ {
		candidate := renderPipelineWithOmittedUpdates(header, groups, omitted)
		if len(candidate) <= maxBytes {
			return candidate
		}
	}

	if candidate := renderPipelineWithTruncatedLatestUpdate(header, groups, maxBytes); candidate != "" {
		return candidate
	}

	return pipelineOmissionSectionWithinLimit(header, totalUnits, maxBytes)
}

func minimumPipelineOmissionSection(pipelineMD string) string {
	header, updates := splitPipelineSectionHeader(pipelineMD)
	totalUnits := countPipelineUpdateUnits(parsePipelineUpdateGroups(updates))
	return header + pipelineUpdatesOmissionMarker(totalUnits) + "\n"
}

func minimumPipelineRetainingLatestUpdate(pipelineMD string) string {
	header, updates := splitPipelineSectionHeader(pipelineMD)
	groups := parsePipelineUpdateGroups(updates)
	totalUnits := countPipelineUpdateUnits(groups)
	if totalUnits == 0 {
		return ""
	}

	group, unit, ok := latestPipelineUpdateUnit(groups)
	if !ok {
		return ""
	}

	omitted := totalUnits - 1
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}
	b.WriteString(group.header)

	unitBudget := len(unit)
	if unitBudget > minLatestPipelineUpdateBytes {
		unitBudget = minLatestPipelineUpdateBytes + len("\n\n") + len(pipelineLatestUpdateTruncationMarker())
	}
	if group.footer != "" {
		unitBudget += len("\n\n") + len(group.footer)
	}

	return renderPipelineWithTruncatedLatestUpdate(header, groups, b.Len()+unitBudget)
}

func pipelineOmissionSectionWithinLimit(header string, omitted, maxBytes int) string {
	markerOnly := header + pipelineUpdatesOmissionMarker(omitted) + "\n"
	if len(markerOnly) <= maxBytes {
		return markerOnly
	}
	if len(header) <= maxBytes {
		return header
	}
	return ""
}

func pipelineSectionHeader(pipelineMD string) string {
	header, _ := splitPipelineSectionHeader(pipelineMD)
	return header
}

func splitPipelineSectionHeader(pipelineMD string) (string, string) {
	const heading = "## Pipeline\n\n"
	if !strings.HasPrefix(pipelineMD, heading) {
		return "", pipelineMD
	}

	rest := pipelineMD[len(heading):]
	introEnd := strings.Index(rest, "\n\n")
	if introEnd < 0 {
		return heading, rest
	}

	headerEnd := len(heading) + introEnd + len("\n\n")
	// The generated attestation is data, not an update detail. Keep it in the
	// fixed header so PR-body truncation never drops the machine-readable
	// snapshot while omitting older human-readable update rounds.
	rest = pipelineMD[headerEnd:]
	if strings.HasPrefix(rest, pipelineAttestationCommentPrefix) {
		if end := strings.Index(rest, pipelineAttestationCommentClosingToken); end >= 0 {
			headerEnd += end + len(pipelineAttestationCommentClosingToken)
			if strings.HasPrefix(pipelineMD[headerEnd:], "\n\n") {
				headerEnd += len("\n\n")
			}
		}
	}
	return pipelineMD[:headerEnd], pipelineMD[headerEnd:]
}

func parsePipelineUpdateGroups(updates string) []pipelineUpdateGroup {
	var groups []pipelineUpdateGroup
	rest := updates
	for strings.TrimSpace(rest) != "" {
		rest = strings.TrimLeft(rest, "\n")
		if strings.HasPrefix(rest, "<details>") {
			end := strings.Index(rest, "</details>")
			if end >= 0 {
				end += len("</details>")
				if end < len(rest) && rest[end] == '\n' {
					end++
				}
				groups = append(groups, parsePipelineDetailsGroup(rest[:end]))
				rest = rest[end:]
				continue
			}
		}
		if strings.HasPrefix(rest, "### ") {
			end := nextPipelineFoldStart(rest[4:])
			if end >= 0 {
				end += 4
			} else {
				end = len(rest)
			}
			groups = append(groups, parsePipelineHeadingGroup(rest[:end]))
			rest = rest[end:]
			continue
		}

		nextFold := nextPipelineFoldStart(rest)
		raw := rest
		if nextFold >= 0 {
			raw = rest[:nextFold]
			rest = rest[nextFold:]
		} else {
			rest = ""
		}
		units := splitPipelineUpdateUnits(raw)
		if len(units) > 0 {
			groups = append(groups, pipelineUpdateGroup{units: units})
		}
	}
	return groups
}

func nextPipelineFoldStart(rest string) int {
	detailsAt := strings.Index(rest, "\n<details>")
	headingAt := strings.Index(rest, "\n### ")
	switch {
	case detailsAt < 0:
		return headingAt
	case headingAt < 0:
		return detailsAt
	case detailsAt < headingAt:
		return detailsAt
	default:
		return headingAt
	}
}

func parsePipelineHeadingGroup(raw string) pipelineUpdateGroup {
	lineEnd := strings.Index(raw, "\n")
	if lineEnd < 0 {
		return pipelineUpdateGroup{header: raw}
	}
	contentStart := lineEnd + 1
	if strings.HasPrefix(raw[contentStart:], "\n") {
		contentStart++
	}
	return pipelineUpdateGroup{
		header: raw[:contentStart],
		units:  splitPipelineUpdateUnits(raw[contentStart:]),
	}
}

func parsePipelineDetailsGroup(raw string) pipelineUpdateGroup {
	footerStart := strings.LastIndex(raw, "</details>")
	summaryEnd := strings.Index(raw, "</summary>")
	if footerStart < 0 || summaryEnd < 0 || summaryEnd > footerStart {
		return pipelineUpdateGroup{units: splitPipelineUpdateUnits(raw)}
	}

	contentStart := summaryEnd + len("</summary>")
	if strings.HasPrefix(raw[contentStart:], "\n\n") {
		contentStart += len("\n\n")
	} else if strings.HasPrefix(raw[contentStart:], "\n") {
		contentStart++
	}

	footerEnd := footerStart + len("</details>")
	if footerEnd < len(raw) && raw[footerEnd] == '\n' {
		footerEnd++
	}

	return pipelineUpdateGroup{
		header: raw[:contentStart],
		units:  splitPipelineUpdateUnits(raw[contentStart:footerStart]),
		footer: raw[footerStart:footerEnd],
	}
}

func splitPipelineUpdateUnits(content string) []string {
	var units []string
	var b strings.Builder
	for _, line := range strings.SplitAfter(content, "\n") {
		b.WriteString(line)
		if strings.TrimSpace(line) != "" {
			continue
		}
		if strings.TrimSpace(b.String()) == "" {
			b.Reset()
			continue
		}
		units = append(units, b.String())
		b.Reset()
	}
	if strings.TrimSpace(b.String()) != "" {
		units = append(units, b.String())
	}
	return units
}

func countPipelineUpdateUnits(groups []pipelineUpdateGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.units)
	}
	return total
}

func renderPipelineWithOmittedUpdates(header string, groups []pipelineUpdateGroup, omitted int) string {
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}

	remainingOmitted := omitted
	wroteGroup := false
	for _, group := range groups {
		if remainingOmitted >= len(group.units) {
			remainingOmitted -= len(group.units)
			continue
		}

		start := remainingOmitted
		remainingOmitted = 0
		units := group.units[start:]
		if len(units) == 0 {
			continue
		}
		if wroteGroup {
			b.WriteString("\n")
		}
		b.WriteString(group.header)
		for _, unit := range units {
			b.WriteString(unit)
		}
		if group.footer != "" {
			last := units[len(units)-1]
			if !strings.HasSuffix(last, "\n\n") {
				if !strings.HasSuffix(last, "\n") {
					b.WriteString("\n")
				}
				b.WriteString("\n")
			}
		}
		b.WriteString(group.footer)
		wroteGroup = true
	}

	return b.String()
}

func renderPipelineWithTruncatedLatestUpdate(header string, groups []pipelineUpdateGroup, maxBytes int) string {
	group, unit, ok := latestPipelineUpdateUnit(groups)
	if !ok {
		return ""
	}

	totalUnits := countPipelineUpdateUnits(groups)
	omitted := totalUnits - 1
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}
	b.WriteString(group.header)
	prefix := b.String()

	footerSeparatorBytes := 0
	if group.footer != "" {
		footerSeparatorBytes = len("\n\n")
	}
	unitBudget := maxBytes - len(prefix) - len(group.footer) - footerSeparatorBytes
	if unitBudget <= 0 {
		return ""
	}

	marker := pipelineLatestUpdateTruncationMarker()
	truncatedUnit := truncatePipelineUpdateAtLineBoundary(unit, unitBudget, marker)
	if truncatedUnit == "" {
		return ""
	}

	candidate := prefix + truncatedUnit
	if group.footer != "" {
		if !strings.HasSuffix(truncatedUnit, "\n\n") {
			if !strings.HasSuffix(truncatedUnit, "\n") {
				candidate += "\n"
			}
			candidate += "\n"
		}
		candidate += group.footer
	}
	if len(candidate) <= maxBytes {
		return candidate
	}
	return ""
}

func latestPipelineUpdateUnit(groups []pipelineUpdateGroup) (pipelineUpdateGroup, string, bool) {
	for i := len(groups) - 1; i >= 0; i-- {
		group := groups[i]
		for j := len(group.units) - 1; j >= 0; j-- {
			if strings.TrimSpace(group.units[j]) == "" {
				continue
			}
			return group, group.units[j], true
		}
	}
	return pipelineUpdateGroup{}, "", false
}

func pipelineUpdatesOmissionMarker(omitted int) string {
	rounds := "rounds"
	if omitted == 1 {
		rounds = "round"
	}
	return fmt.Sprintf("_... (%d earlier update %s omitted to keep the managed validation comment within its %d KiB limit; full history is in the run log.)_", omitted, rounds, scm.MaxManagedPRCommentBytes/1024)
}

func pipelineLatestUpdateTruncationMarker() string {
	return fmt.Sprintf("_... (latest pipeline update truncated to keep the managed validation comment within its %d KiB limit; full history is in the run log.)_", scm.MaxManagedPRCommentBytes/1024)
}

func truncateTextAtLineBoundary(text string, maxBytes int, marker string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	if marker != "" {
		marker = "\n\n" + marker
	}
	available := maxBytes - len(marker)
	if available <= 0 {
		if len(marker) <= maxBytes {
			return strings.TrimLeft(marker, "\n")
		}
		return ""
	}

	available = utf8BoundaryBefore(text, available)
	cut := strings.LastIndex(text[:available], "\n")
	if cut <= 0 {
		cut = available
	}
	return strings.TrimRight(text[:cut], "\n") + marker
}

func truncatePipelineUpdateAtLineBoundary(text string, maxBytes int, marker string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	if marker != "" {
		marker = "\n\n" + marker
	}
	available := maxBytes - len(marker)
	if available <= 0 {
		if len(marker) <= maxBytes {
			return strings.TrimLeft(marker, "\n")
		}
		return ""
	}

	available = utf8BoundaryBefore(text, available)
	searchEnd := available
	if searchEnd < len(text) && text[searchEnd] == '\n' {
		searchEnd++
	}
	cut := strings.LastIndex(text[:searchEnd], "\n")
	if cut <= 0 {
		return strings.TrimRight(text[:available], "\n") + marker
	}
	return strings.TrimRight(text[:cut], "\n") + marker
}

func utf8BoundaryBefore(text string, n int) int {
	if n >= len(text) {
		return len(text)
	}
	if n <= 0 {
		return 0
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return n
}

func stripGeneratedSections(body string) string {
	if body == "" {
		return ""
	}

	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	skipping := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw)

		if skipping {
			if strings.HasPrefix(line, "## ") {
				if isGeneratedSectionHeading(line) {
					continue
				}
				skipping = false
			} else {
				continue
			}
		}

		if isGeneratedSectionHeading(line) {
			skipping = true
			continue
		}

		out = append(out, raw)
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}

func isGeneratedSectionHeading(line string) bool {
	if !strings.HasPrefix(strings.TrimSpace(line), "##") {
		return false
	}

	heading := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "##"))
	heading = strings.TrimRight(heading, ":.!? ")
	heading = strings.ToLower(heading)

	switch heading {
	case "intent", "risk assessment", "testing", "tests", "pipeline":
		return true
	default:
		return false
	}
}

func fallbackPRContent(sctx *pipeline.StepContext, finalDiff string, bodyLimit int) (prContent, error) {
	title, err := renderPRTitle(sctx, "update pull request")
	if err != nil {
		return prContent{}, err
	}
	// A drafting failure must not dump a path/status listing, command output, or
	// validation history into the squash-commit body. The detailed deterministic
	// evidence still publishes through the managed validation comment.
	body := "Updates the final branch delta."
	if strings.TrimSpace(finalDiff) == "" {
		body = "Updates the branch; a generated scope summary was unavailable."
	}
	if bodyLimit > 0 && scm.PRBodyLen(body) > bodyLimit {
		body = scm.ClampPRBody(body, bodyLimit)
	}
	return prContent{Title: title, Body: body}, nil
}
