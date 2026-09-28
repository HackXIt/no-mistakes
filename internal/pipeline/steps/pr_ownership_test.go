package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func ownedFixture(t *testing.T) (prContent, string) {
	t.Helper()
	appendix := "## Risk Assessment\n\nLow recorded risk.\n\n## Testing\n\nRecorded evidence link.\n\n" + compliantPipelineBody(t, testPipelineHeadSHA)
	content, err := composeOwnedPRContent(prOwnedBody{before: "## Testing\n\n- [x] Human checked this\n\nCloses test/repo#7"}, "feat: story", appendix, 0, scm.ProviderUnknown)
	if err != nil {
		t.Fatal(err)
	}
	return content, appendix
}

func TestPROwnershipNeverInfersFromHeadings(t *testing.T) {
	t.Parallel()
	body := "Mention no-mistakes-pr-appendix by name without claiming ownership.\n\n## Intent\n\nAuthor intent\n\n## Risk Assessment\n\nAuthor risk\n\n## Tests\n\n- [x] Human checkbox\n\n## Pipeline\n\nRelease plan\nCloses test/repo#1"
	parts, err := parsePROwnedBody(body)
	if err != nil || parts.managed || parts.before != body {
		t.Fatalf("heading resemblance claimed author content: %+v, %v", parts, err)
	}
	_, appendix := ownedFixture(t)
	got, err := composeOwnedPRContent(parts, "", appendix, 0, scm.ProviderUnknown)
	if err != nil || !strings.HasPrefix(got.Body, body) {
		t.Fatalf("author headings removed: %s, %v", got.Body, err)
	}
}

func TestPROwnershipRejectsAmbiguityAndEdits(t *testing.T) {
	t.Parallel()
	content, _ := ownedFixture(t)
	cases := map[string]string{
		"edited evidence":     strings.Replace(content.Body, "Low recorded risk.", "Low recorded risk. Human note.", 1),
		"missing end":         strings.Replace(content.Body, prAppendixEnd, "", 1),
		"missing start":       strings.Replace(content.Body, prAppendixStart, "", 1),
		"malformed digest":    strings.Replace(content.Body, "sha256=", "sha256=z", 1),
		"empty payload":       prAppendixStart + strings.Repeat("0", 64) + " -->\n" + prAppendixEnd,
		"truncated":           prAppendixStart,
		"reversed":            prAppendixEnd + "\n" + prAppendixStart,
		"duplicate":           content.Body + "\n" + content.Body,
		"quoted block":        "```markdown\n" + content.Body + "\n```",
		"tilde quote":         "~~~~\n" + content.Body + "\n~~~~",
		"indented marker":     strings.Replace(content.Body, prAppendixStart, "    "+prAppendixStart, 1),
		"spacing change":      strings.Replace(content.Body, prAppendixStart, "<!--  no-mistakes-pr-appendix:v1 sha256=", 1),
		"inline marker":       strings.Replace(content.Body, prAppendixStart, "quote "+prAppendixStart, 1),
		"foreign attestation": content.Body + "\n" + buildPipelineAttestation(nil, nil, testPipelineHeadSHA),
		"unowned legacy":      compliantPipelineBody(t, testPipelineHeadSHA),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePROwnedBody(body); err == nil {
				t.Fatal("ambiguous/editable content was claimed as generated")
			}
		})
	}
}

func TestPROwnershipFailsSizePressureWithoutTruncation(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	for _, parts := range []prOwnedBody{
		{before: strings.Repeat("author\n", maxPullRequestBodyBytes)},
		{before: "author", after: strings.Repeat("closing refs\n", maxPullRequestBodyBytes), managed: true},
	} {
		if _, err := composeOwnedPRContent(parts, "", appendix, 0, scm.ProviderUnknown); err == nil {
			t.Fatal("oversized author content was truncated")
		}
	}
	if _, err := composeOwnedPRContent(prOwnedBody{before: "author"}, "", appendix+strings.Repeat("evidence\n", maxPullRequestBodyBytes), 0, scm.ProviderUnknown); err == nil {
		t.Fatal("oversized evidence was dropped")
	}
	if _, err := composeOwnedPRContent(prOwnedBody{before: strings.Repeat("😀", 100)}, "", appendix, 400, scm.ProviderUnknown); err == nil {
		t.Fatal("provider character cap ignored")
	}
}

func TestPROwnershipPublicationRedactionPrecedesDigest(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	appendix += "\n\nEvidence /Users/private/evidence.png\n```text\n" + prAppendixEnd + "\n```"
	got, err := composeOwnedPRContent(prOwnedBody{before: "## Overview\n\n/home/person/work\n\n", after: "\nC:\\Users\\person\\notes", managed: true}, "feat: /Users/person/path", appendix, 0, scm.ProviderUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"/Users/", "/home/person", `C:\Users\person`} {
		if strings.Contains(got.Title+got.Body, private) {
			t.Fatalf("published home path %q", private)
		}
	}
	if _, err := parsePROwnedBody(got.Body); err != nil {
		t.Fatalf("redaction invalidated integrity guard: %v", err)
	}
	if strings.Count(got.Body, pipelineAttestationCommentPrefix) != 1 || strings.Count(got.Body, prAppendixNamespace) != 2 {
		t.Fatal("quoted evidence markers shadowed actual ownership/attestation")
	}
}

type ownershipRaceHost struct {
	scm.Host
	body       string
	title      string
	allowTitle bool
	reads      int
	writes     int
	read       func(*ownershipRaceHost) error
	writeError error
	afterWrite string
	provider   scm.Provider
}

func (h *ownershipRaceHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	h.reads++
	if h.read != nil {
		if err := h.read(h); err != nil {
			return scm.PRContent{}, err
		}
	}
	title := h.title
	if title == "" {
		title = "Author title"
	}
	return scm.PRContent{Title: title, Body: h.body}, nil
}

func (h *ownershipRaceHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	h.writes++
	if content.Title != "" {
		if !h.allowTitle {
			return nil, errors.New("must leave author title alone")
		}
		if h.provider == scm.ProviderGitLab && titleOwnershipValue(h.provider, h.title) != h.title && titleOwnershipValue(h.provider, content.Title) == content.Title {
			h.title = "Draft: " + content.Title
		} else {
			h.title = content.Title
		}
	}
	if h.writeError != nil {
		return nil, h.writeError
	}
	h.body = content.Body + h.afterWrite
	return pr, nil
}

func TestPROwnershipUpdateMergesLatestAuthorEdits(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	latest := strings.Replace(content.Body, "Human checked this", "Human updated checkbox label", 1) + "\n\nFixes test/other#9"
	host := &ownershipRaceHost{body: content.Body, read: func(h *ownershipRaceHost) error {
		if h.reads == 1 {
			h.body = latest
		}
		return nil
	}}
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent(content), "", "", false, appendix+"\nNew recorded fact.", 0, scm.ProviderUnknown); err != nil {
		t.Fatal(err)
	}
	if host.writes != 1 || !strings.Contains(host.body, "Human updated checkbox label") || !strings.HasSuffix(host.body, "Fixes test/other#9") || !strings.Contains(host.body, "New recorded fact.") {
		t.Fatalf("lost latest body: writes=%d, %s", host.writes, host.body)
	}
}

func TestPROwnershipRefreshesGeneratedContentAndPreservesHumanEdits(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	generated, err := composeOwnedPRContent(prOwnedBody{
		before:             "First generated narrative.",
		generatedNarrative: true,
		generatedTitle:     true,
	}, "feat: first generated title", appendix, 0, scm.ProviderUnknown)
	if err != nil {
		t.Fatal(err)
	}

	host := &ownershipRaceHost{body: generated.Body, title: generated.Title, allowTitle: true}
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent(generated), "feat: second generated title", "Second generated narrative.", true, appendix, 0, scm.ProviderUnknown); err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(host.body)
	if err != nil {
		t.Fatal(err)
	}
	if parts.before != "Second generated narrative.\n\n" || host.title != "feat: second generated title" || !parts.generatedNarrative || !parts.generatedTitle {
		t.Fatalf("generated content did not refresh: parts=%+v title=%q", parts, host.title)
	}

	humanBody := strings.Replace(generated.Body, "First generated narrative.", "Human-edited narrative.", 1)
	human := &ownershipRaceHost{body: humanBody, title: "Human-edited title", allowTitle: true}
	if err := updateOwnedPR(sctx, human, &scm.PR{Number: "42"}, scm.PRContent{Title: human.title, Body: human.body}, "feat: replacement title", "Replacement narrative.", true, appendix, 0, scm.ProviderUnknown); err != nil {
		t.Fatal(err)
	}
	parts, err = parsePROwnedBody(human.body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parts.before, "Human-edited narrative.") || human.title != "Human-edited title" || parts.generatedNarrative || parts.generatedTitle {
		t.Fatalf("human-owned content was replaced or retained generated ownership: parts=%+v title=%q", parts, human.title)
	}
}

func TestPROwnershipRefreshesGitLabDraftTitles(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	generated, err := composeOwnedPRContent(prOwnedBody{
		before:             "First generated narrative.",
		generatedNarrative: true,
		generatedTitle:     true,
	}, "feat: first generated title", appendix, 0, scm.ProviderGitLab)
	if err != nil {
		t.Fatal(err)
	}

	host := &ownershipRaceHost{
		body:       generated.Body,
		title:      "Draft: " + generated.Title,
		allowTitle: true,
		provider:   scm.ProviderGitLab,
	}
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent{Title: host.title, Body: host.body}, "feat: second generated title", "Second generated narrative.", true, appendix, 0, scm.ProviderGitLab); err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(host.body)
	if err != nil || host.title != "Draft: feat: second generated title" || !generatedTitleOwned(parts, host.title, scm.ProviderGitLab) {
		t.Fatalf("GitLab draft title did not refresh or settle: parts=%+v title=%q err=%v", parts, host.title, err)
	}

	host.title = "Draft: Human-edited title"
	sctx.Config = &config.Config{PR: config.PR{TitleFormat: "[{title}]"}}
	if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent{Title: host.title, Body: host.body}, "fix: configured title", "Third generated narrative.", true, appendix, 0, scm.ProviderGitLab); err != nil {
		t.Fatal(err)
	}
	if host.title != "Draft: fix: configured title" {
		t.Fatalf("configured title did not retain authority: %q", host.title)
	}
}

func TestPROwnershipUpdateFailuresNeverReadAsSuccess(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	for _, mode := range []string{"read-error", "write-error", "verify-error", "verify-divergence", "keeps-changing", "edited-owned", "size"} {
		t.Run(mode, func(t *testing.T) {
			host := &ownershipRaceHost{body: content.Body}
			initial := content
			wantWrites := 0
			switch mode {
			case "read-error":
				host.read = func(*ownershipRaceHost) error { return errors.New("read unavailable") }
			case "write-error":
				host.writeError = errors.New("uncertain write")
				wantWrites = 1
			case "verify-error":
				host.read = func(h *ownershipRaceHost) error {
					if h.writes > 0 {
						return errors.New("verify unavailable")
					}
					return nil
				}
				wantWrites = 1
			case "verify-divergence":
				host.afterWrite = "\nConcurrent author note"
				wantWrites = 1
			case "keeps-changing":
				host.read = func(h *ownershipRaceHost) error { h.body += fmt.Sprintf("\nAuthor edit %d", h.reads); return nil }
			case "edited-owned":
				initial.Body = strings.Replace(content.Body, "Low recorded risk.", "Human note inside evidence", 1)
			case "size":
				initial.Body = strings.Repeat("Author content\n", maxPullRequestBodyBytes)
			}
			err := updateOwnedPR(&pipeline.StepContext{Ctx: context.Background()}, host, &scm.PR{Number: "42"}, scm.PRContent(initial), "", "", false, appendix+"\nNew fact", 0, scm.ProviderUnknown)
			if err == nil || host.writes != wantWrites {
				t.Fatalf("err=%v, writes=%d want %d", err, host.writes, wantWrites)
			}
		})
	}
}

func TestPROwnershipRestampPreservesAuthorsAndConsumerContract(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	parts, err := parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	parts.generatedNarrative = true
	parts.generatedTitle = true
	content, err = composeOwnedPRContent(parts, "feat: generated title", appendix, 0, scm.ProviderUnknown)
	if err != nil {
		t.Fatal(err)
	}
	content.Body += "\n\nFixes test/other#9"
	parts, err = parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	newHead := strings.Repeat("ab", 20)
	host := &attestationTestHost{body: content.Body, title: "Author's title"}
	if err := restampPRAttestation(context.Background(), host, &scm.PR{Number: "42"}, newHead, nil); err != nil {
		t.Fatal(err)
	}
	rebound, err := parsePROwnedBody(host.body)
	if err != nil || rebound.before != parts.before || rebound.after != parts.after || !rebound.generatedNarrative || !rebound.generatedTitle || rebound.generatedTitleSHA != parts.generatedTitleSHA || host.title != "Author's title" {
		t.Fatalf("restamp invalidated ownership or author content: %+v, %v", rebound, err)
	}
	if got, out := runVerifyPy(t, content.Body, newHead); got != "failure" {
		t.Fatalf("stale head passed: %s", out)
	}
	if got, out := runVerifyPy(t, host.body, newHead); got != "success" {
		t.Fatalf("restamped body failed consumer: %s", out)
	}
	host.body = strings.Replace(host.body, "Low recorded risk.", "Human note inside evidence", 1)
	host.updates = 0
	if err := restampPRAttestation(context.Background(), host, &scm.PR{Number: "42"}, testPipelineHeadSHA, nil); err == nil || host.updates != 0 {
		t.Fatalf("edited evidence overwritten during restamp: err=%v, writes=%d", err, host.updates)
	}
}
