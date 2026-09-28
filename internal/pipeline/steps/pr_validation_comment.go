package steps

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	validationCommentNamespace = "no-mistakes-validation-comment"
	validationCommentStart     = "<!-- " + validationCommentNamespace + ":v1 sha256="
	validationCommentEnd       = "<!-- /" + validationCommentNamespace + ":v1 -->"
	validationCommentHeading   = "## Validation evidence"
	validationTruncationMarker = "\n\n…(validation evidence truncated)"
)

var validationCommentMarkerPattern = regexp.MustCompile(`(?i)<!--\s*/?\s*` + validationCommentNamespace)

type ownedValidationComment struct {
	comment scm.PRComment
	content string
}

// renderValidationComment keeps detailed human evidence out of the concise PR
// description. The machine attestation remains in the description; removing it
// here also ensures copied evidence can never look like a second enforcement
// record to consumers that inspect comments.
func (s *PRStep) renderValidationComment(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	pipelineMD, risk, testing := s.buildPipelineSectionFor(sctx, provider, false)
	pipelineMD = stripPipelineAttestation(pipelineMD)
	inner := joinAppendixSections(risk, testing, pipelineMD)
	intent := ""
	if published := publicPRIntent(sctx); published != "" {
		intent = "## Intent\n\n" + neutralizeAttestationMarkers(published)
	}
	content := joinBlocks(validationCommentHeading, intent, inner)
	budget := scm.MaxManagedPRCommentBytes - len(wrapValidationComment(""))
	if budget <= 0 {
		return "", fmt.Errorf("managed validation comment budget is invalid")
	}
	if len(content) > budget {
		content = truncatePRBodySections(content, budget, validationTruncationMarker)
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

func stripPipelineAttestation(text string) string {
	if marker := extractPipelineAttestationMarker(text); marker != "" {
		text = strings.Replace(text, marker, "", 1)
	}
	text = strings.Replace(text, noMistakesPRSignature, "", 1)
	return strings.TrimSpace(text)
}

func wrapValidationComment(content string) string {
	return fmt.Sprintf("%s%x -->\n%s\n%s", validationCommentStart, sha256.Sum256([]byte(content)), content, validationCommentEnd)
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

func findOwnedValidationComment(comments []scm.PRComment) (*ownedValidationComment, error) {
	var found *ownedValidationComment
	for _, comment := range comments {
		owned, ok, err := parseValidationComment(comment)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if strings.TrimSpace(comment.ID) == "" {
			return nil, fmt.Errorf("managed validation comment has no durable provider identity")
		}
		if found != nil {
			return nil, fmt.Errorf("multiple pipeline-owned validation comments found; refusing an ambiguous update")
		}
		copy := owned
		found = &copy
	}
	return found, nil
}

// publishValidationComment is idempotent and fail-closed. A write that returns
// an error is never blindly replayed: the comment list is re-read first, so an
// ambiguously applied create/update can settle without creating a duplicate.
// Exact read-after-write verification also catches provider truncation and a
// comment moved to a sibling review object.
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

	comments, err := commentsHost.ListPRComments(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("list validation comments: %w", err)
	}
	owned, err := findOwnedValidationComment(comments)
	if err != nil {
		return err
	}
	if owned != nil && owned.comment.Body == body {
		return nil
	}

	if owned == nil {
		_, writeErr := commentsHost.CreatePRComment(sctx.Ctx, pr, body)
		if err := verifyValidationComment(sctx, commentsHost, pr, body); err == nil {
			return nil
		} else if writeErr != nil {
			return fmt.Errorf("create validation comment: %w (settlement check: %v)", writeErr, err)
		} else {
			return fmt.Errorf("verify created validation comment: %w", err)
		}
	}

	// Providers expose full comment writes rather than compare-and-swap. Re-read
	// immediately before the write and refuse if this exact owned object moved.
	latest, err := commentsHost.ListPRComments(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("re-read validation comments before update: %w", err)
	}
	current, err := findOwnedValidationComment(latest)
	if err != nil {
		return err
	}
	if current == nil || current.comment.ID != owned.comment.ID || current.comment.Body != owned.comment.Body {
		return fmt.Errorf("validation comment changed while preparing update; no write performed")
	}
	_, writeErr := commentsHost.UpdatePRComment(sctx.Ctx, pr, owned.comment.ID, body)
	if err := verifyValidationComment(sctx, commentsHost, pr, body); err == nil {
		return nil
	} else if writeErr != nil {
		return fmt.Errorf("update validation comment: %w (settlement check: %v)", writeErr, err)
	} else {
		return fmt.Errorf("verify updated validation comment: %w", err)
	}
}

func verifyValidationComment(sctx *pipeline.StepContext, host scm.ManagedPRCommentHost, pr *scm.PR, body string) error {
	comments, err := host.ListPRComments(sctx.Ctx, pr)
	if err != nil {
		return err
	}
	owned, err := findOwnedValidationComment(comments)
	if err != nil {
		return err
	}
	if owned == nil || owned.comment.Body != body {
		return fmt.Errorf("provider did not settle the proposed validation comment")
	}
	return nil
}
