package steps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	prAppendixNamespace       = "no-mistakes-pr-appendix"
	prAppendixStart           = "<!-- " + prAppendixNamespace + ":v1 sha256="
	prAppendixEnd             = "<!-- /" + prAppendixNamespace + ":v1 -->"
	prGeneratedOwnershipAttrs = " narrative_sha256=%s title_sha256=%s"
)

var (
	prAppendixMarkerPattern           = regexp.MustCompile(`(?i)<!--\s*/?\s*` + prAppendixNamespace)
	prOwnershipAttrsPattern           = regexp.MustCompile(`^ narrative_sha256=([0-9a-f]{64}|-) title_sha256=([0-9a-f]{64}|-)$`)
	legacyPipelineOmissionPattern     = regexp.MustCompile(`^_\.\.\. \([1-9][0-9]* earlier update rounds? omitted to keep the PR body within GitHub's 65536-char limit; full history is in the run log\.\)_`)
	legacyConversationOmissionPattern = regexp.MustCompile(`^[1-9][0-9]* further review question\(s\) omitted for length\.$`)
	legacyFindingSummaryPattern       = regexp.MustCompile(`^[1-9][0-9]* (?:errors?|warnings?|infos?|issues?)(?: \((?:[1-9][0-9]* (?:errors?|warnings?|infos?))(?:, [1-9][0-9]* (?:errors?|warnings?|infos?))*\))?$`)
	legacyFixSummaryPattern           = regexp.MustCompile(`^[1-9][0-9]* issues? found(?: → (?:auto-fixed|no changes applied|fix attempted; result not reported)(?: \((?:[2-9]|[1-9][0-9]+)\))?)* ✅$`)
)

func hasPRAppendixMarkers(body string) bool {
	return prAppendixMarkerPattern.MatchString(body)
}

type prOwnedBody struct {
	before, appendix, after string
	managed                 bool
	generatedNarrative      bool
	generatedTitle          bool
	generatedTitleSHA       string
}

// The digest is an accidental-edit/ownership guard, NOT authentication. PR
// authors can edit every byte, including the attestation. We never infer
// ownership from headings. The delimited appendix is replaceable only while
// intact; optional hashes also identify unchanged generated title/narrative.
func parsePROwnedBody(body string) (prOwnedBody, error) {
	if !hasPRAppendixMarkers(body) {
		if strings.Contains(body, pipelineAttestationCommentPrefix) {
			return prOwnedBody{}, fmt.Errorf("existing PR has an unowned attestation; refusing to infer ownership")
		}
		return prOwnedBody{before: body}, nil
	}
	start := strings.Index(body, prAppendixStart)
	end := strings.Index(body, prAppendixEnd)
	if len(prAppendixMarkerPattern.FindAllStringIndex(body, -1)) != 2 || start < 0 || end < start || (start > 0 && body[start-1] != '\n') || markdownFenceOpen(body[:max(start, 0)]) {
		return prOwnedBody{}, fmt.Errorf("ambiguous PR appendix ownership markers; refusing to discard author content")
	}
	digestStart := start + len(prAppendixStart)
	headerEndOffset := strings.Index(body[digestStart:], " -->\n")
	if headerEndOffset < 64 {
		return prOwnedBody{}, fmt.Errorf("malformed PR appendix ownership markers")
	}
	headerEnd := digestStart + headerEndOffset
	contentStart := headerEnd + len(" -->\n")
	afterEnd := end + len(prAppendixEnd)
	if contentStart >= end || body[end-1] != '\n' || (afterEnd < len(body) && body[afterEnd] != '\n') {
		return prOwnedBody{}, fmt.Errorf("malformed PR appendix ownership markers")
	}
	parts := prOwnedBody{before: body[:start], appendix: body[contentStart : end-1], after: body[afterEnd:], managed: true}
	attrs := body[digestStart+64 : headerEnd]
	digestPayload := parts.appendix
	if attrs != "" {
		digestPayload = attrs + "\n" + digestPayload
	}
	if markdownFenceOpen(parts.appendix) || fmt.Sprintf("%x", sha256.Sum256([]byte(digestPayload))) != body[digestStart:digestStart+64] {
		return prOwnedBody{}, fmt.Errorf("PR appendix was edited or is malformed; refusing to overwrite possible author content")
	}
	if attrs != "" {
		match := prOwnershipAttrsPattern.FindStringSubmatch(attrs)
		if match == nil {
			return prOwnedBody{}, fmt.Errorf("malformed generated PR ownership metadata")
		}
		if match[1] != "-" && fmt.Sprintf("%x", sha256.Sum256([]byte(parts.before))) == match[1] {
			parts.generatedNarrative = true
		}
		if match[2] != "-" {
			parts.generatedTitle = true
			parts.generatedTitleSHA = match[2]
		}
	}
	if strings.Contains(parts.before+parts.after, pipelineAttestationCommentPrefix) || strings.Count(parts.appendix, pipelineAttestationCommentPrefix) != 1 {
		return prOwnedBody{}, fmt.Errorf("ambiguous PR attestation ownership")
	}
	marker := strings.SplitN(parts.appendix, pipelineAttestationCommentPrefix, 2)[1]
	payload, _, ok := strings.Cut(marker, pipelineAttestationCommentClosingToken)
	var attestation pipelineAttestation
	if !ok || json.Unmarshal([]byte(payload), &attestation) != nil || attestation.HeadSHA == "" {
		return prOwnedBody{}, fmt.Errorf("malformed attestation in PR appendix")
	}
	return parts, nil
}

// Only track fenced blocks to keep a quoted marker from acquiring ownership.
// This deliberately does not interpret headings or attempt general Markdown
// rewriting. Four-space-indented lines cannot open a CommonMark fence.
func markdownFenceOpen(text string) bool {
	var fence markdownFence
	for _, line := range strings.Split(text, "\n") {
		fence.consume(line)
	}
	return fence.marker != 0
}

type markdownFence struct {
	marker byte
	width  int
}

func (f *markdownFence) consume(raw string) {
	line := strings.TrimLeft(raw, " ")
	if len(raw)-len(line) > 3 || len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return
	}
	if f.marker == 0 {
		// CommonMark forbids backticks in a backtick fence's info string.
		if line[0] == '`' && strings.ContainsRune(line[n:], '`') {
			return
		}
		f.marker, f.width = line[0], n
	} else if line[0] == f.marker && n >= f.width && strings.TrimSpace(line[n:]) == "" {
		f.marker, f.width = 0, 0
	}
}

func generatedTitleOwned(parts prOwnedBody, title string, provider scm.Provider) bool {
	title = titleOwnershipValue(provider, title)
	return parts.generatedTitle && fmt.Sprintf("%x", sha256.Sum256([]byte(title))) == parts.generatedTitleSHA
}

func hasGeneratedPRContent(parts prOwnedBody, current scm.PRContent, provider scm.Provider) bool {
	return parts.generatedNarrative || generatedTitleOwned(parts, current.Title, provider)
}

func titleOwnershipValue(provider scm.Provider, title string) string {
	if provider != scm.ProviderGitLab {
		return title
	}
	trimmed := strings.TrimSpace(title)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"draft:", "[draft]", "(draft)"} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return title
}

func titleUpdateSettled(provider scm.Provider, current, proposed, verified string) bool {
	expected := proposed
	if provider == scm.ProviderGitLab && titleOwnershipValue(provider, current) != current && titleOwnershipValue(provider, proposed) == proposed {
		expected = "Draft: " + proposed
	}
	return verified == expected
}

func wrapPRAppendix(appendix string) string {
	return fmt.Sprintf("%s%x -->\n%s\n%s", prAppendixStart, sha256.Sum256([]byte(appendix)), appendix, prAppendixEnd)
}

func wrapOwnedPRAppendix(parts prOwnedBody, title, appendix string, provider scm.Provider) string {
	attrs := ""
	if parts.generatedNarrative || parts.generatedTitle {
		narrativeSHA := "-"
		if parts.generatedNarrative {
			narrativeSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(parts.before)))
		}
		titleSHA := "-"
		if parts.generatedTitle {
			titleSHA = parts.generatedTitleSHA
			if title != "" {
				titleSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(titleOwnershipValue(provider, title))))
			}
			if titleSHA == "" {
				titleSHA = "-"
			}
		}
		attrs = fmt.Sprintf(prGeneratedOwnershipAttrs, narrativeSHA, titleSHA)
	}
	digestPayload := appendix
	if attrs != "" {
		digestPayload = attrs + "\n" + digestPayload
	}
	return fmt.Sprintf("%s%x%s -->\n%s\n%s", prAppendixStart, sha256.Sum256([]byte(digestPayload)), attrs, appendix, prAppendixEnd)
}

func canonicalLegacyReviewConversation(section string) bool {
	const heading = "### Review conversation\n\n"
	if !strings.HasPrefix(section, heading) {
		return false
	}
	lines := strings.Split(strings.TrimPrefix(section, heading), "\n")
	entries := 0
	for i := 0; i < len(lines); {
		if lines[i] == "" && i+1 == len(lines) {
			break
		}
		if lines[i] == "" && i+2 == len(lines) && legacyConversationOmissionPattern.MatchString(lines[i+1]) {
			return entries > 0
		}
		if !strings.HasPrefix(lines[i], "- **Q:** ") || len(strings.TrimPrefix(lines[i], "- **Q:** ")) == 0 || i+1 >= len(lines) {
			return false
		}
		answer := lines[i+1]
		validAnswer := strings.HasPrefix(answer, "  **A** (") && strings.Contains(answer, ")**:** ")
		validAnswer = validAnswer || strings.HasPrefix(answer, "  **Unanswered:** ") || strings.HasPrefix(answer, "  **Withdrawn by the reviewer:** ")
		if !validAnswer {
			return false
		}
		entries++
		i += 2
	}
	return entries > 0
}

func canonicalLegacyStepSummary(summary string) bool {
	separator := strings.Index(summary, " **")
	if separator <= 0 {
		return false
	}
	emoji := summary[:separator]
	rest := summary[separator+len(" **"):]
	nameEnd := strings.Index(rest, "** - ")
	if nameEnd <= 0 || strings.ContainsAny(rest[:nameEnd], "*\r\n") {
		return false
	}
	outcome := rest[nameEnd+len("** - "):]
	switch emoji {
	case "⏳":
		return outcome == "pending" || outcome == "running"
	case "⏸️":
		return outcome == "awaiting approval" || outcome == "review fix"
	case "🔄":
		return outcome == "auto-fixing"
	case "❌":
		return outcome == "failed"
	case "⏭️":
		return outcome == "skipped"
	case "✅":
		return outcome == "passed"
	case "🚨":
		return outcome == "high risk"
	case "⚠️":
		return outcome == "findings unavailable" || outcome == "medium risk" || legacyFindingSummaryPattern.MatchString(outcome)
	case "🔧":
		return legacyFixSummaryPattern.MatchString(outcome)
	default:
		return false
	}
}

func canonicalLegacyPipelineSuffix(suffix string) bool {
	if suffix == "" || suffix == "\n\n" {
		return true
	}
	if !strings.HasPrefix(suffix, "\n\n") {
		return false
	}
	rest := strings.TrimPrefix(suffix, "\n\n")
	omitted := false
	if match := legacyPipelineOmissionPattern.FindString(rest); match != "" {
		omitted = true
		rest = strings.TrimPrefix(rest, match)
		if rest == "" || rest == "\n" {
			return true
		}
		if !strings.HasPrefix(rest, "\n\n") {
			return false
		}
		rest = strings.TrimPrefix(rest, "\n\n")
	}

	details := 0
	for strings.HasPrefix(rest, "<details>\n<summary>") {
		summaryStart := len("<details>\n<summary>")
		summaryEnd := strings.Index(rest, "</summary>\n\n")
		if summaryEnd < summaryStart || !canonicalLegacyStepSummary(rest[summaryStart:summaryEnd]) {
			return false
		}
		contentStart := summaryEnd + len("</summary>\n\n")
		closeAt := strings.Index(rest[contentStart:], "\n</details>")
		if closeAt < 0 || strings.TrimSpace(rest[contentStart:contentStart+closeAt]) == "" {
			return false
		}
		rest = rest[contentStart+closeAt+len("\n</details>"):]
		details++
		if rest == "" || rest == "\n" {
			return true
		}
		if strings.HasPrefix(rest, "\n\n<details>") {
			rest = strings.TrimPrefix(rest, "\n\n")
			continue
		}
		break
	}
	if details == 0 && !omitted {
		return false
	}
	if strings.HasPrefix(rest, "\n\n\n### Review conversation\n\n") {
		return canonicalLegacyReviewConversation(strings.TrimPrefix(rest, "\n\n\n"))
	}
	if omitted && strings.HasPrefix(rest, "### Review conversation\n\n") {
		return canonicalLegacyReviewConversation(rest)
	}
	return false
}

// parseOrMigratePROwnedBody adopts only the exact legacy body contract: one
// generated signature plus one live attestation. The marker moves into the new
// compact owned trailer while every other byte (including author edits and the
// old visible validation history) is retained. A bare/foreign attestation is
// never claimed. Legacy detail can then be removed explicitly by the author;
// heading inference would risk deleting their text.
func parseOrMigratePROwnedBody(body string) (prOwnedBody, error) {
	if hasPRAppendixMarkers(body) || !strings.Contains(body, pipelineAttestationCommentPrefix) {
		return parsePROwnedBody(body)
	}
	if strings.Count(body, pipelineAttestationCommentPrefix) != 1 || strings.Count(body, noMistakesPRSignature) != 1 {
		return prOwnedBody{}, fmt.Errorf("ambiguous unowned PR attestation; refusing migration")
	}
	pipelinePrefix := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n"
	minimalPrefix := noMistakesPRSignature + "\n\n"
	footerPrefix := pipelinePrefix
	footerStart := strings.LastIndex(body, "\n"+pipelinePrefix)
	minimal := false
	if footerStart >= 0 {
		footerStart++
	} else if strings.HasPrefix(body, pipelinePrefix) {
		footerStart = 0
	} else {
		footerPrefix = minimalPrefix
		footerStart = strings.LastIndex(body, "\n\n"+minimalPrefix)
		if footerStart >= 0 {
			footerStart += 2
		} else if strings.HasPrefix(body, minimalPrefix) {
			footerStart = 0
		} else {
			return prOwnedBody{}, fmt.Errorf("unowned PR attestation is not in a canonical legacy footer")
		}
		minimal = true
	}
	if strings.Count(body, footerPrefix) != 1 || markdownFenceOpen(body[:footerStart]) {
		return prOwnedBody{}, fmt.Errorf("unowned PR attestation is not in a canonical legacy footer")
	}
	markerStart := footerStart + len(footerPrefix)
	if !strings.HasPrefix(body[markerStart:], pipelineAttestationCommentPrefix) {
		return prOwnedBody{}, fmt.Errorf("unowned PR attestation is not in a canonical legacy footer")
	}
	marker := extractPipelineAttestationMarker(body[markerStart:])
	if marker == "" {
		return prOwnedBody{}, fmt.Errorf("malformed unowned PR attestation; refusing migration")
	}
	markerEnd := markerStart + len(marker)
	if (minimal && markerEnd != len(body)) || (!minimal && !canonicalLegacyPipelineSuffix(body[markerEnd:])) {
		return prOwnedBody{}, fmt.Errorf("unowned PR attestation is not in the trailing legacy footer")
	}
	var attestation pipelineAttestation
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, pipelineAttestationCommentPrefix), pipelineAttestationCommentClosingToken)
	if json.Unmarshal([]byte(payload), &attestation) != nil || strings.TrimSpace(attestation.HeadSHA) == "" {
		return prOwnedBody{}, fmt.Errorf("malformed unowned PR attestation; refusing migration")
	}
	migrated := body[:footerStart]
	if !minimal {
		migrated += "## Pipeline\n\n" + body[markerEnd:]
	}
	return prOwnedBody{before: migrated}, nil
}

// composeOwnedPRContent uses the same publication redaction owner as ordinary
// drafting, BEFORE stamping the byte-integrity guard. No clamp or heading-based
// stripper may run here: if author text and all recorded evidence cannot fit,
// publication fails. That includes suffix text/closing lines added after the
// generated block. Model-authored copies of ownership markers in evidence are
// escaped before the actual delimiters are inserted, like foreign attestations.
func composeOwnedPRContent(parts prOwnedBody, title, appendix string, bodyLimit int, provider scm.Provider) (prContent, error) {
	before := redactPRContent(prContent{Body: parts.before}).Body
	after := redactPRContent(prContent{Body: parts.after}).Body
	appendix = prAppendixMarkerPattern.ReplaceAllStringFunc(appendix, func(marker string) string {
		// Break the namespace without rewriting ordinary mentions of its name.
		i := strings.LastIndex(marker, "-")
		return marker[:i] + "\\" + marker[i:]
	})
	appendix = redactPRContent(prContent{Body: appendix}).Body
	if !parts.managed && before != "" {
		before += "\n\n"
	}
	title = redactPRContent(prContent{Title: title}).Title
	parts.before = before
	parts.after = after
	content := prContent{Title: title, Body: before + wrapOwnedPRAppendix(parts, title, appendix, provider) + after}
	if err := validateOwnedPRBudget(content.Body, bodyLimit); err != nil {
		return prContent{}, err
	}
	if _, err := parsePROwnedBody(content.Body); err != nil {
		return prContent{}, err
	}
	return content, nil
}

func validateOwnedPRBudget(body string, bodyLimit int) error {
	if len(body) > maxPullRequestBodyBytes || (bodyLimit > 0 && scm.PRBodyLen(body) > bodyLimit) {
		return fmt.Errorf("PR body exceeds provider budget; refusing to drop author text, closing references or recorded evidence")
	}
	return nil
}

// updateOwnedPR re-reads before the full-body write and verifies afterwards.
// This is NOT compare-and-swap: providers expose full-body writes. Detected
// pre-write edits are merged from their latest version (bounded); a write error
// or post-write divergence fails without replaying a potentially applied write.
func updateOwnedPR(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, initial scm.PRContent, title, narrative string, generated bool, appendix string, bodyLimit int, provider scm.Provider) error {
	reader, ok := host.(scm.PRContentReader)
	if !ok {
		return fmt.Errorf("provider cannot read PR content; author-safe publication updates are unsupported")
	}
	current := initial
	for attempt := 0; attempt < 3; attempt++ {
		parts, err := parseOrMigratePROwnedBody(current.Body)
		if err != nil {
			return err
		}
		writeTitle := title
		titleOwned := generatedTitleOwned(parts, current.Title, provider)
		configuredTitle := title != "" && sctx.Config != nil && sctx.Config.PR.TitleFormat != ""
		if parts.generatedTitle && !titleOwned {
			parts.generatedTitle = false
			parts.generatedTitleSHA = ""
		}
		if generated {
			if current.Body == "" && !parts.managed {
				parts.before = narrative
				parts.generatedNarrative = true
				parts.generatedTitle = title != ""
			} else {
				if parts.generatedNarrative {
					parts.before = strings.TrimRight(narrative, "\n") + "\n\n"
				}
				if !titleOwned && !configuredTitle {
					writeTitle = ""
				}
			}
		} else if current.Body == "" && !parts.managed {
			parts.before = narrative
		}
		content, err := composeOwnedPRContent(parts, writeTitle, appendix, bodyLimit, provider)
		if err != nil {
			return err
		}
		latest, err := reader.GetPRContent(sctx.Ctx, pr)
		if err != nil {
			return fmt.Errorf("re-read PR before publication update: %w", err)
		}
		if latest.Body != current.Body || latest.Title != current.Title {
			current = latest
			continue
		}
		if content.Body != current.Body || content.Title != "" {
			if _, err := host.UpdatePR(sctx.Ctx, pr, scm.PRContent(content)); err != nil {
				return fmt.Errorf("update PR content: %w", err)
			}
		}
		verified, err := reader.GetPRContent(sctx.Ctx, pr)
		if err != nil {
			return fmt.Errorf("verify PR publication update: %w", err)
		}
		if verified.Body != content.Body || (content.Title != "" && !titleUpdateSettled(provider, current.Title, content.Title, verified.Title)) {
			return fmt.Errorf("PR content changed or update did not settle; refusing to report successful publication")
		}
		return nil
	}
	return fmt.Errorf("PR content kept changing before template update; no write performed")
}

// Restamping is also an owner-authorized appendix edit. Recompute its integrity
// guard without touching author text, while refusing evidence edited by anyone
// else. Legacy unmarked bodies retain the existing restamp contract.
func rebindOwnedPRAttestation(body, head string, steps []*db.StepResult, policy pipelineAttestationPolicy) (string, bool, error) {
	if !hasPRAppendixMarkers(body) {
		updated, rebound := rebindPipelineAttestationWithSteps(body, head, steps, policy)
		return updated, rebound, nil
	}
	parts, err := parsePROwnedBody(body)
	if err != nil {
		return "", false, err
	}
	appendix, rebound := rebindPipelineAttestationWithSteps(parts.appendix, head, steps, policy)
	if !rebound {
		return "", false, fmt.Errorf("cannot rebind the owned PR attestation")
	}
	updated := parts.before + wrapOwnedPRAppendix(parts, "", appendix, scm.ProviderUnknown) + parts.after
	if len(updated) > maxPullRequestBodyBytes {
		return "", false, fmt.Errorf("restamped PR body exceeds the publication budget")
	}
	return updated, true, nil
}
