package steps

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	validationCommentNamespace = "no-mistakes-validation-comment"
	validationCommentStart     = "<!-- " + validationCommentNamespace + ":v1 sha256="
	validationCommentEnd       = "<!-- /" + validationCommentNamespace + ":v1 -->"
	validationCommentHeading   = "## Validation evidence"
	validationCommentHeadLabel = "Validated head"
	validationTruncationMarker = "\n\n…(validation evidence truncated)"
	validationSettlementReads  = 3
	validationSettlementDelay  = 100 * time.Millisecond
)

var validationCommentMarkerPattern = regexp.MustCompile(`(?i)<!--\s*/?\s*` + validationCommentNamespace)

type ownedValidationComment struct {
	comment scm.PRComment
	content string
}

// renderValidationComment keeps detailed human evidence out of the concise PR
// description. The machine attestation remains in the description; removing it
// here also ensures copied evidence can never look like a second enforcement
// record to consumers that inspect comments. The ordinary no-mistakes link is
// retained as human provenance for the Pipeline section.
func (s *PRStep) renderValidationComment(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	steps, rounds, err := loadPRPipelineRecords(sctx)
	if err != nil {
		return "", err
	}
	pipelineMD, risk, testing := s.buildPipelineSectionFromRecords(sctx, provider, steps, rounds)
	pipelineMD = stripPipelineAttestation(pipelineMD)
	intent := ""
	if published := publicPRIntent(sctx); published != "" {
		intent = "## Intent\n\n" + neutralizeAttestationMarkers(published)
	}
	head := ""
	if sctx != nil && sctx.Run != nil && strings.TrimSpace(sctx.Run.HeadSHA) != "" {
		head = validationCommentHeadLabel + ": `" + strings.TrimSpace(sctx.Run.HeadSHA) + "`"
	}
	budget := scm.MaxManagedPRCommentBytes - len(wrapValidationComment(""))
	if budget <= 0 {
		return "", fmt.Errorf("managed validation comment budget is invalid")
	}
	var content string
	switch appendixMode(sctx) {
	case config.PRAppendixMinimal:
		content = fitMinimalValidationComment(validationCommentHeading, head, intent, risk, budget)
	case config.PRAppendixCollapsed:
		if prBodyFlavorFor(provider) == prBodyHTML {
			content = fitCollapsedValidationComment(validationCommentHeading, head, intent, risk, testing, pipelineMD, budget)
		} else {
			content = fitValidationComment(validationCommentHeading, head, intent, risk, testing, pipelineMD, budget)
		}
	default:
		content = fitValidationComment(validationCommentHeading, head, intent, risk, testing, pipelineMD, budget)
	}
	content = safepath.RedactText(content)
	body := wrapValidationComment(content)
	if len(body) > scm.MaxManagedPRCommentBytes {
		return "", fmt.Errorf("validation comment exceeds the shared provider budget")
	}
	return body, nil
}

func (s *PRStep) renderValidationCommentForHead(sctx *pipeline.StepContext, provider scm.Provider, head string) (string, error) {
	if sctx == nil || sctx.Run == nil || strings.TrimSpace(head) == "" || sctx.Run.HeadSHA == head {
		return s.renderValidationComment(sctx, provider)
	}
	copyContext := *sctx
	copyRun := *sctx.Run
	copyRun.HeadSHA = head
	copyContext.Run = &copyRun
	return s.renderValidationComment(&copyContext, provider)
}

func fitMinimalValidationComment(heading, head, intent, risk string, budget int) string {
	heading = neutralizeValidationCommentMarkers(heading)
	head = neutralizeValidationCommentMarkers(head)
	intent = neutralizeValidationCommentMarkers(intent)
	risk = oneLineRisk(neutralizeAttestationMarkers(neutralizeValidationCommentMarkers(risk)))
	content := joinBlocks(heading, head, intent, risk)
	if len(content) <= budget {
		return content
	}
	if intent != "" {
		intent = "## Intent\n\n" + strings.TrimSpace(validationTruncationMarker)
		content = joinBlocks(heading, head, intent, risk)
		if len(content) <= budget {
			return content
		}
	}
	prefix := joinBlocks(heading, head, intent)
	remaining := budget - len(prefix)
	if prefix != "" && risk != "" {
		remaining -= len("\n\n")
	}
	if remaining > 0 {
		risk = truncateTextAtLineBoundary(risk, remaining, validationTruncationMarker)
		content = joinBlocks(prefix, risk)
		if len(content) <= budget {
			return content
		}
	}
	return truncateTextAtLineBoundary(prefix, budget, validationTruncationMarker)
}

func fitCollapsedValidationComment(heading, head, intent, risk, testing, pipelineMD string, budget int) string {
	heading = neutralizeValidationCommentMarkers(heading)
	head = neutralizeValidationCommentMarkers(head)
	intent = neutralizeValidationCommentMarkers(intent)
	prefix := joinBlocks(heading, head, intent)
	overhead := len(validationDetailsOpen) + len(validationDetailsClose)
	if prefix != "" {
		overhead += len(prefix) + len("\n\n")
	}
	minimumEvidenceBudget := len("## Risk Assessment\n\n") + len(strings.TrimSpace(validationTruncationMarker))
	if budget-overhead < minimumEvidenceBudget && intent != "" {
		intent = "## Intent\n\n" + strings.TrimSpace(validationTruncationMarker)
		prefix = joinBlocks(heading, head, intent)
		overhead = len(validationDetailsOpen) + len(validationDetailsClose) + len(prefix) + len("\n\n")
	}
	if evidenceBudget := budget - overhead; evidenceBudget > 0 {
		evidence := fitValidationComment("", "", "", risk, testing, pipelineMD, evidenceBudget)
		if evidence == "" && len(strings.TrimSpace(validationTruncationMarker)) <= evidenceBudget {
			evidence = strings.TrimSpace(validationTruncationMarker)
		}
		if evidence != "" {
			content := joinBlocks(prefix, wrapValidation(evidence))
			if len(content) <= budget {
				return content
			}
		}
	}
	return fitValidationComment(heading, head, intent, "", "", "", budget)
}

func fitValidationComment(heading, head, intent, risk, testing, pipelineMD string, budget int) string {
	heading = neutralizeValidationCommentMarkers(heading)
	head = neutralizeValidationCommentMarkers(head)
	intent = neutralizeValidationCommentMarkers(intent)
	risk = neutralizeValidationCommentMarkers(risk)
	testing = neutralizeValidationCommentMarkers(testing)
	pipelineMD = neutralizeValidationCommentMarkers(pipelineMD)
	prefix := joinBlocks(heading, head)
	if strings.TrimSpace(risk) != "" {
		risk = "## Risk Assessment\n\n" + neutralizeAttestationMarkers(risk)
	}
	if strings.TrimSpace(testing) != "" {
		testing = neutralizeAttestationMarkers(testing)
	}
	markerText := strings.TrimSpace(validationTruncationMarker)
	markerSection := func(title string) string {
		return title + "\n\n" + markerText
	}
	full := func(pipeline string) string {
		return joinBlocks(prefix, intent, risk, testing, pipeline)
	}
	if content := full(pipelineMD); len(content) <= budget {
		return content
	}

	// Testing can inline logs and artifacts, so shed it before recorded
	// pipeline history. Replacing a whole Markdown section keeps details/fences
	// balanced and makes the omission explicit.
	if testing != "" {
		testing = markerSection("## Testing")
	}
	if content := full(pipelineMD); len(content) <= budget {
		return content
	}

	for {
		other := joinBlocks(prefix, intent, risk, testing)
		remaining := budget - len(other)
		if other != "" && pipelineMD != "" {
			remaining -= len("\n\n")
		}
		if remaining > 0 && pipelineMD != "" {
			pipeline := truncatePipelineSection(pipelineMD, remaining)
			if pipeline == "" {
				pipeline = markerSection("## Pipeline")
				if len(pipeline) > remaining {
					pipeline = ""
				}
			}
			if pipeline != "" {
				if content := joinBlocks(other, pipeline); len(content) <= budget {
					return content
				}
			}
		} else if len(other) <= budget {
			return other
		}

		switch {
		case intent != "" && !strings.Contains(intent, markerText):
			intent = markerSection("## Intent")
		case risk != "" && !strings.Contains(risk, markerText):
			risk = markerSection("## Risk Assessment")
		case testing != "" && !strings.Contains(testing, markerText):
			testing = markerSection("## Testing")
		default:
			return truncateTextAtLineBoundary(other, budget, validationTruncationMarker)
		}
	}
}

func stripPipelineAttestation(text string) string {
	if marker := extractPipelineAttestationMarker(text); marker != "" {
		text = strings.Replace(text, marker, "", 1)
	}
	return strings.TrimSpace(text)
}

func wrapValidationComment(content string) string {
	content = neutralizeValidationCommentMarkers(content)
	return fmt.Sprintf("%s%x -->\n%s\n%s", validationCommentStart, sha256.Sum256([]byte(content)), content, validationCommentEnd)
}

func neutralizeValidationCommentMarkers(content string) string {
	return validationCommentMarkerPattern.ReplaceAllStringFunc(content, func(marker string) string {
		return strings.Replace(marker, "<!--", "<!\\--", 1)
	})
}

func parseValidationComment(comment scm.PRComment) (ownedValidationComment, bool, error) {
	if !validationCommentMarkerPattern.MatchString(comment.Body) {
		return ownedValidationComment{}, false, nil
	}
	start := strings.Index(comment.Body, validationCommentStart)
	end := strings.Index(comment.Body, validationCommentEnd)
	if len(validationCommentMarkerPattern.FindAllStringIndex(comment.Body, -1)) != 2 || start != 0 || end < start {
		return ownedValidationComment{}, false, fmt.Errorf("ambiguous validation comment ownership markers")
	}
	digestStart := len(validationCommentStart)
	contentStart := digestStart + 64 + len(" -->\n")
	if contentStart > end || len(comment.Body) < contentStart || comment.Body[digestStart+64:contentStart] != " -->\n" || end == 0 || comment.Body[end-1] != '\n' || end+len(validationCommentEnd) != len(comment.Body) {
		return ownedValidationComment{}, false, fmt.Errorf("malformed validation comment ownership markers")
	}
	content := comment.Body[contentStart : end-1]
	if fmt.Sprintf("%x", sha256.Sum256([]byte(content))) != comment.Body[digestStart:digestStart+64] {
		return ownedValidationComment{}, false, fmt.Errorf("validation comment was edited; refusing to overwrite possible human content")
	}
	return ownedValidationComment{comment: comment, content: content}, true, nil
}

func findBoundValidationComment(comments []scm.PRComment, commentID string) (*ownedValidationComment, error) {
	var found *ownedValidationComment
	for _, comment := range comments {
		if strings.TrimSpace(comment.ID) != commentID {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("provider returned the bound validation comment more than once")
		}
		owned, ok, err := parseValidationComment(comment)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bound validation comment no longer carries its ownership markers")
		}
		copy := owned
		found = &copy
	}
	return found, nil
}

func refuseUnboundValidationMarkers(comments []scm.PRComment) error {
	for _, comment := range comments {
		if validationCommentMarkerPattern.MatchString(comment.Body) {
			return fmt.Errorf("validation comment ownership marker has no persisted provider identity")
		}
	}
	return nil
}

func managedValidationCommentBinding(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) (*db.ManagedPRCommentBinding, string, error) {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || strings.TrimSpace(sctx.Run.RepoID) == "" {
		return nil, "", fmt.Errorf("validation comment publication requires durable repository state")
	}
	number := strings.TrimSpace(pr.Number)
	if number == "" {
		var err error
		number, err = scm.ExtractPRNumber(pr.URL)
		if err != nil || strings.TrimSpace(number) == "" {
			return nil, "", fmt.Errorf("validation comment publication requires an exact pull request number")
		}
	}
	binding, err := sctx.DB.GetManagedPRCommentBinding(sctx.Run.RepoID, string(host.Provider()), number)
	if err != nil {
		return nil, "", err
	}
	if binding != nil && !samePRIdentity(binding.PRURL, pr) {
		return nil, "", fmt.Errorf("persisted validation comment belongs to sibling pull request %s", binding.PRURL)
	}
	return binding, number, nil
}

// publishValidationComment is idempotent and fail-closed. A successful create
// persists its exact provider ID before settlement; a create error cannot prove
// ownership and is not replayed. An update error is reconciled only against the
// already-bound ID. Exact read-after-write verification also catches provider
// truncation and a comment moved to a sibling review object.
func publishValidationComment(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, body string) error {
	if host == nil || pr == nil {
		return fmt.Errorf("validation comment publication requires a pull request identity")
	}
	if err := requireOwnedPRIdentity(sctx, pr); err != nil {
		return err
	}
	if !host.Capabilities().ManagedPRComments {
		return fmt.Errorf("%s cannot maintain the pipeline validation comment: %w", host.Provider(), scm.ErrUnsupported)
	}
	commentsHost, ok := host.(scm.ManagedPRCommentHost)
	if !ok {
		return fmt.Errorf("%s advertises managed PR comments without implementing them", host.Provider())
	}
	if len(body) > scm.MaxManagedPRCommentBytes {
		return fmt.Errorf("validation comment exceeds the shared provider budget")
	}

	binding, prNumber, err := managedValidationCommentBinding(sctx, host, pr)
	if err != nil {
		return err
	}
	comments, err := commentsHost.ListPRComments(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("list validation comments: %w", err)
	}
	if binding == nil {
		if err := refuseUnboundValidationMarkers(comments); err != nil {
			return err
		}
		created, err := commentsHost.CreatePRComment(sctx.Ctx, pr, body)
		if err != nil {
			return fmt.Errorf("create validation comment: %w", err)
		}
		if strings.TrimSpace(created.ID) == "" || created.Body != body {
			return fmt.Errorf("create validation comment returned an incomplete settled identity")
		}
		if _, ok, err := parseValidationComment(created); err != nil {
			return fmt.Errorf("create validation comment returned invalid ownership: %w", err)
		} else if !ok {
			return fmt.Errorf("create validation comment returned no ownership markers")
		}
		binding = &db.ManagedPRCommentBinding{
			RepoID: sctx.Run.RepoID, Provider: string(host.Provider()), PRNumber: prNumber,
			PRURL: pr.URL, CommentID: created.ID,
		}
		if err := sctx.DB.BindManagedPRComment(*binding); err != nil {
			return err
		}
		if err := verifyValidationComment(sctx, commentsHost, pr, binding.CommentID, body); err != nil {
			return fmt.Errorf("verify created validation comment: %w", err)
		}
		return nil
	}

	owned, err := findBoundValidationComment(comments, binding.CommentID)
	if err != nil {
		return err
	}
	if owned == nil {
		return fmt.Errorf("persisted validation comment %s is missing; refusing to create a replacement", binding.CommentID)
	}
	if owned.comment.Body == body {
		return nil
	}

	// Providers expose full comment writes rather than compare-and-swap. Re-read
	// immediately before the write and refuse if this exact owned object moved.
	latest, err := commentsHost.ListPRComments(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("re-read validation comments before update: %w", err)
	}
	current, err := findBoundValidationComment(latest, binding.CommentID)
	if err != nil {
		return err
	}
	if current == nil || current.comment.Body != owned.comment.Body {
		return fmt.Errorf("validation comment changed while preparing update; no write performed")
	}
	updated, writeErr := commentsHost.UpdatePRComment(sctx.Ctx, pr, binding.CommentID, body)
	if writeErr == nil && (updated.ID != binding.CommentID || updated.Body != body) {
		return fmt.Errorf("update validation comment returned a different settled identity")
	}
	if err := verifyValidationComment(sctx, commentsHost, pr, binding.CommentID, body); err == nil {
		return nil
	} else if writeErr != nil {
		return fmt.Errorf("update validation comment: %w (settlement check: %v)", writeErr, err)
	} else {
		return fmt.Errorf("verify updated validation comment: %w", err)
	}
}

func verifyValidationComment(sctx *pipeline.StepContext, host scm.ManagedPRCommentHost, pr *scm.PR, commentID, body string) error {
	var lastErr error
	for attempt := 0; attempt < validationSettlementReads; attempt++ {
		comments, err := host.ListPRComments(sctx.Ctx, pr)
		if err != nil {
			lastErr = err
		} else {
			owned, err := findBoundValidationComment(comments, commentID)
			if err != nil {
				return err
			}
			if owned != nil && owned.comment.Body == body {
				return nil
			}
			lastErr = fmt.Errorf("provider did not settle the proposed validation comment")
		}
		if attempt+1 < validationSettlementReads {
			timer := time.NewTimer(validationSettlementDelay)
			select {
			case <-sctx.Ctx.Done():
				timer.Stop()
				return sctx.Ctx.Err()
			case <-timer.C:
			}
		}
	}
	return lastErr
}
