//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const staleTwoFileEvidence = "Inspected only final files: internal/example/flag.go and cmd/example/main.go."

func writeFinalPRScopeScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "final-pr-scope-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      summary: "review clean"
      risk_level: medium
      risk_rationale: "medium risk because only two source files changed"
      risk_scope: source-or-external
  - match: "You are validating a code change by driving the product itself. Derive the scenarios this change must satisfy, then run each one against the real running product."
    text: "two-file test evidence"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "` + staleTwoFileEvidence + `"
      testing_summary: "Focused validation passed at the test step target commit."
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
  - match: "Perform the combined documentation and lint housekeeping pass for this change."
    text: "documentation updated"
    edits:
      - path: "docs/flag.md"
        new: "# Flag\n"
      - path: "docs/reference.md"
        new: "# Reference\n"
    structured:
      findings: []
      summary: "update flag documentation"
  - match: "Draft a pull request title and summary for the full branch delta."
    text: "full four-file PR summary"
    structured:
      title: "feat: add example flag"
      body: |
        ## What Changed

        - Add flag behavior in ` + "`internal/example/flag.go`" + ` and CLI wiring in ` + "`cmd/example/main.go`" + `.
        - Add documentation in ` + "`docs/flag.md`" + ` and ` + "`docs/reference.md`" + `.
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write final PR scope scenario: %v", err)
	}
	return path
}

// TestPRDescriptionIsConciseWhileEvidenceStaysStepScoped drives the real
// pipeline through GitHub publication and verifies the two public surfaces:
// the description is squash-commit-style narrative plus the compact
// enforcement trailer, while the single managed comment keeps the detailed
// step-scoped risk, testing, and pipeline evidence.
func TestPRDescriptionIsConciseWhileEvidenceStaysStepScoped(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeFinalPRScopeScenario(t)})
	ctx := context.Background()

	parentURL := "https://github.com/example/no-mistakes.git"
	forkURL := "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-final-pr-scope.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init with fork URL: %v\n%s", err, out)
	}

	const branch = "feature/final-pr-scope"
	h.CommitChange(branch, "internal/example/flag.go", "package example\n", "add flag behavior")
	preDocumentHead := h.CommitChange(branch, "cmd/example/main.go", "package main\n", "add flag CLI")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	if run.HeadSHA == preDocumentHead {
		t.Fatalf("Document did not advance the tested head %s", preDocumentHead)
	}

	finalHead, err := h.runGit(ctx, forkDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read final fork head: %v\n%s", err, finalHead)
	}
	if got := strings.TrimSpace(string(finalHead)); got != run.HeadSHA {
		t.Fatalf("final fork head = %s, want run head %s", got, run.HeadSHA)
	}
	finalDiff, err := h.runGit(ctx, forkDir, "diff", "--name-only", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read final branch diff: %v\n%s", err, finalDiff)
	}
	wantFiles := []string{
		"cmd/example/main.go",
		"docs/flag.md",
		"docs/reference.md",
		"internal/example/flag.go",
	}
	if got := strings.Fields(string(finalDiff)); !equalStrings(got, wantFiles) {
		t.Fatalf("final branch files = %q, want %q", got, wantFiles)
	}

	testPrompt := findInvocationContaining(h.AgentInvocations(), "You are validating a code change")
	if !strings.Contains(testPrompt, "target commit: "+preDocumentHead) {
		t.Fatalf("Test evidence was not bound to its pre-Document target %s:\n%s", preDocumentHead, testPrompt)
	}
	prPrompt := findInvocationContaining(h.AgentInvocations(), "Draft a pull request title and summary for the full branch delta.")
	for _, want := range append([]string{"target commit: " + run.HeadSHA}, wantFiles...) {
		if !strings.Contains(prPrompt, want) {
			t.Fatalf("final PR drafting prompt missing %q:\n%s", want, prPrompt)
		}
	}

	invocations := readGHStubInvocations(t, ghLog)
	body := createdPRBody(t, invocations)
	comment := validationCommentAfterPRCreate(t, invocations)
	for _, want := range wantFiles {
		if !strings.Contains(body, want) {
			t.Fatalf("concise PR description missing final-diff claim %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"## What Changed", "## Risk Assessment", "## Testing", "## Pipeline", staleTwoFileEvidence} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("concise PR description contains detailed content %q:\n%s", forbidden, body)
		}
	}
	if strings.Count(body, "<!-- no-mistakes-pipeline-attestation:v1 ") != 1 {
		t.Fatalf("description must retain exactly one enforcement attestation:\n%s", body)
	}

	for _, want := range []string{
		"## Validation evidence",
		"Validated head: `" + run.HeadSHA + "`",
		"## Risk Assessment",
		"medium risk because only two source files changed",
		"## Testing",
		"Focused validation passed at the test step target commit.",
		"## Pipeline",
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("managed validation comment missing %q:\n%s", want, comment)
		}
	}
	if strings.Contains(comment, "<!-- no-mistakes-pipeline-attestation:v1 ") {
		t.Fatalf("managed validation comment must not duplicate the enforcement record:\n%s", comment)
	}
}

func validationCommentAfterPRCreate(t *testing.T, invocations []ghStubInvocation) string {
	t.Helper()
	seenCreate := false
	for _, inv := range invocations {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			seenCreate = true
			continue
		}
		if seenCreate && len(inv.Args) > 0 && inv.Args[0] == "api" && inv.Body != "" && strings.Contains(inv.Body, "## Validation evidence") {
			return inv.Body
		}
	}
	t.Fatalf("no managed validation-comment write after PR create in %+v", invocations)
	return ""
}

func createdPRBody(t *testing.T, invocations []ghStubInvocation) string {
	t.Helper()
	for _, inv := range invocations {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			if inv.Body == "" {
				t.Fatalf("PR create did not receive a body on stdin: %+v", inv)
			}
			return inv.Body
		}
	}
	t.Fatalf("no PR create invocation in %+v", invocations)
	return ""
}

func equalStrings(got, want []string) bool {
	return bytes.Equal([]byte(strings.Join(got, "\n")), []byte(strings.Join(want, "\n")))
}
