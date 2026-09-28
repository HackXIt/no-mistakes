// Command fakecli is the tiny helper that pipeline-step tests place on PATH as
// gh/glab/git. It implements the same FAKE_CLI_MODE handlers that used to live
// in the race-instrumented test binary, so each fake invocation is a small
// process spawn instead of re-execing that binary (~0.8s under -race).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Logging and stateful PR readback consume the same command stdin, not two
// independent streams. Each helper subprocess represents exactly one command.
var readFakeBody = sync.OnceValues(func() ([]byte, error) { return io.ReadAll(os.Stdin) })

func main() {
	mode := os.Getenv("FAKE_CLI_MODE")
	if mode == "" {
		os.Exit(1)
	}
	handleFakeCLI(mode)
}

func handleFakeCLI(mode string) {
	args := os.Args[1:]
	logFile := os.Getenv("FAKE_CLI_LOG")

	if logFile != "" {
		f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintln(f, strings.Join(args, " "))
			f.Close()
		}
	}
	logFakeCLIStdinBody(args, logFile)

	switch mode {
	case "gh":
		fakeGHHandler(args)
	case "glab":
		fakeGlabHandler(args)
	case "record-success":
		fakeRecordSuccessHandler()
	case "git-passthrough":
		fakeGitPassthroughHandler(args)
	case "git-move-head-passthrough":
		fakeGitMoveHeadPassthroughHandler(args)
	case "git-intervening-push-passthrough":
		fakeGitInterveningPushPassthroughHandler(args)
	case "git-reset-after-commit-passthrough":
		fakeGitResetAfterCommitPassthroughHandler(args)
	case "git-require-noninteractive-env":
		fakeGitRequireNonInteractiveEnvHandler(args)
	case "git-status-error":
		fakeGitStatusErrorHandler(args)
	case "git-stale-dirty-status":
		fakeGitStaleDirtyStatusHandler(args)
	case "git-commit-error":
		fakeGitCommitErrorHandler(args)
	case "git-remote-error":
		fakeGitRemoteErrorHandler(args)
	case "ci-gh":
		fakeCIGHHandler(args)
	case "ci-gh-seq":
		fakeCIGHSequenceHandler(args)
	case "ci-gh-nochecks":
		fakeCIGHNoChecksHandler(args)
	case "ci-glab":
		fakeCIGlabHandler(args)
	case "ci-glab-seq":
		fakeCIGlabSequenceHandler(args)
	case "ci-gh-reconcile":
		fakeCIGHReconcileHandler(args)
	case "ci-gh-with-intervening-push":
		// A single step invocation can need both a faked gh (for the PR
		// attestation write) and a faked git (to inject a push-time race) in
		// the same sctx.Env, so this dispatches on the binary name rather
		// than a second, mutually exclusive FAKE_CLI_MODE.
		binaryName := filepath.Base(os.Args[0])
		if strings.TrimSuffix(binaryName, filepath.Ext(binaryName)) == "git" {
			fakeGitInterveningPushPassthroughHandler(args)
		} else {
			fakeCIGHHandler(args)
		}
	default:
		os.Exit(1)
	}
}

func logFakeCLIStdinBody(args []string, logFile string) {
	if logFile == "" || !argsUseStdinBodyFile(args) {
		return
	}
	body, err := readFakeBody()
	if err != nil {
		return
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	label := "stdin --body "
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--input" && args[i+1] == "-" {
			label = "stdin --comment "
			break
		}
	}
	fmt.Fprint(f, label)
	fmt.Fprintln(f, string(body))
}

func argsUseStdinBodyFile(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if (args[i] == "--body-file" || args[i] == "--input") && args[i+1] == "-" {
			return true
		}
	}
	return false
}

func fakeRecordSuccessHandler() {
	logFile := os.Getenv("FAKE_CLI_LOG")
	if logFile != "" {
		f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintln(f, filepath.Base(os.Args[0]))
			f.Close()
		}
	}
	os.Exit(0)
}

func fakeGHHandler(args []string) {
	fakeGHHandlePRContentCommands(args, strings.Join(args, " "))
	prURL := os.Getenv("FAKE_CLI_PR_URL")
	prBase := os.Getenv("FAKE_CLI_PR_BASE")
	prListJSON, hasPRListJSON := os.LookupEnv("FAKE_CLI_PR_LIST_JSON")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
		if hasPRListJSON {
			fmt.Print(prListJSON)
			os.Exit(0)
		}
		if prURL == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		// A real `gh pr list --base X` filters server-side by the PR's actual
		// base, so an existing PR opened against a different base than the one
		// requested here must not be returned.
		if requestedBase, ok := fakeCLIFlagValue(args, "--base"); ok && prBase != "" && requestedBase != prBase {
			fmt.Println("[]")
			os.Exit(0)
		}
		number := extractTrailingNumber(prURL)
		fmt.Printf("[{\"number\":%d,\"url\":%q,\"baseRefName\":%q}]\n", number, prURL, prBase)
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
		if strings.Contains(strings.Join(args, " "), "--json state") {
			state := os.Getenv("FAKE_CLI_PR_STATE")
			if state == "" {
				state = "OPEN"
			}
			fmt.Println(state)
			os.Exit(0)
		}
		if prURL != "" {
			fmt.Println(prURL)
			os.Exit(0)
		}
		os.Exit(1)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "edit" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "create" {
		fakeGHStorePRBody(args)
		repo, _ := fakeCLIFlagValue(args, "--repo")
		if repo == "" {
			repo = "test/repo"
		}
		if parts := strings.Split(repo, "/"); len(parts) == 3 {
			repo = strings.Join(parts[1:], "/")
		}
		fmt.Printf("https://github.com/%s/pull/99\n", repo)
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeGitStatusErrorHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
		fmt.Fprintln(os.Stderr, "status failed")
		os.Exit(1)
	}
	fakeGitForward(args, realGit)
}

func fakeGitStaleDirtyStatusHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
		fmt.Println(" M feature.txt")
		os.Exit(0)
	}
	fakeGitForward(args, realGit)
}

func fakeGitCommitErrorHandler(args []string) {
	if len(args) > 0 && args[0] == "commit" {
		fmt.Fprintln(os.Stderr, "intentional commit failure")
		os.Exit(1)
	}
	fakeGitForward(args, os.Getenv("FAKE_CLI_REAL_GIT"))
}

func fakeGitPassthroughHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	fakeGitForward(args, realGit)
}

func fakeGitMoveHeadPassthroughHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if len(args) > 0 && args[0] == "push" {
		replacementHead := os.Getenv("FAKE_CLI_REPLACEMENT_HEAD")
		if replacementHead == "" {
			fmt.Fprintln(os.Stderr, "missing FAKE_CLI_REPLACEMENT_HEAD")
			os.Exit(1)
		}
		cmd := exec.Command(realGit, "reset", "--hard", replacementHead)
		cmd.Stdout = io.Discard
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fakeGitForward(args, realGit)
}

// fakeGitInterveningPushPassthroughHandler models a genuine push-time race:
// right before forwarding the pipeline's own push, it pushes an
// already-prepared interloper commit to the same remote ref from a second
// clone, so the pipeline's real force-with-lease push - resolved against the
// remote state as it was at decision time, moments earlier - is rejected by
// git's own lease check exactly as it would be against a real concurrent
// push. FAKE_CLI_INTERLOPER_DIR is the second clone (with the interloper
// commit already committed but not yet pushed); FAKE_CLI_INTERLOPER_REMOTE
// and FAKE_CLI_INTERLOPER_REF name where to push it.
func fakeGitInterveningPushPassthroughHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if fakeGitSubcommand(args) == "push" {
		interloperDir := os.Getenv("FAKE_CLI_INTERLOPER_DIR")
		interloperRemote := os.Getenv("FAKE_CLI_INTERLOPER_REMOTE")
		interloperRef := os.Getenv("FAKE_CLI_INTERLOPER_REF")
		if interloperDir != "" && interloperRemote != "" && interloperRef != "" {
			push := exec.Command(realGit, "-C", interloperDir, "push", interloperRemote, interloperRef)
			push.Stdout = io.Discard
			push.Stderr = os.Stderr
			if err := push.Run(); err != nil {
				fmt.Fprintln(os.Stderr, "interloper push failed:", err)
				os.Exit(1)
			}
		}
	}
	fakeGitForward(args, realGit)
}

// fakeGitSubcommand returns the git subcommand in args, skipping the global
// options (and their values) that may precede it.
func fakeGitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return ""
}

// fakeGitResetAfterCommitPassthroughHandler forwards every git invocation to the
// real binary and, for a commit, resets HEAD afterwards. It models an
// out-of-band reset landing between the pipeline's own commit and the head
// continuity check that follows it, without relying on a repository hook.
func fakeGitResetAfterCommitPassthroughHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if fakeGitSubcommand(args) != "commit" {
		fakeGitForward(args, realGit)
		return
	}
	replacementHead := os.Getenv("FAKE_CLI_REPLACEMENT_HEAD")
	if replacementHead == "" {
		fmt.Fprintln(os.Stderr, "missing FAKE_CLI_REPLACEMENT_HEAD")
		os.Exit(1)
	}
	commit := exec.Command(realGit, args...)
	commit.Stdout = os.Stdout
	commit.Stderr = os.Stderr
	if err := commit.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status := exitErr.ExitCode(); status >= 0 {
				os.Exit(status)
			}
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	reset := exec.Command(realGit, "reset", "--hard", replacementHead)
	reset.Stdout = io.Discard
	reset.Stderr = os.Stderr
	if err := reset.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeGitRequireNonInteractiveEnvHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	required := map[string]string{
		"GIT_EDITOR":          "true",
		"GIT_SEQUENCE_EDITOR": "true",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_OPTIONAL_LOCKS":  "0",
	}
	for key, want := range required {
		if got := os.Getenv(key); got != want {
			fmt.Fprintf(os.Stderr, "%s=%q, want %q\n", key, got, want)
			os.Exit(1)
		}
	}
	helperCmd := exec.Command(realGit, "config", "--global", "--get-all", "credential.https://github.com.helper")
	helperCmd.Env = os.Environ()
	helperOut, err := helperCmd.Output()
	if err != nil || !strings.Contains(string(helperOut), "gh auth git-credential") {
		fmt.Fprintf(os.Stderr, "github credential helper = %q, err=%v; want inherited gh auth git-credential helper\n", strings.TrimSpace(string(helperOut)), err)
		os.Exit(1)
	}
	fakeGitForward(args, realGit)
}

func fakeGitRemoteErrorHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if len(args) > 0 && (args[0] == "ls-remote" || args[0] == "push") {
		fmt.Fprintf(os.Stderr, "remote failed: %s\n", strings.Join(args, " "))
		os.Exit(1)
	}
	fakeGitForward(args, realGit)
}

func fakePRHeadSHA() string {
	configured := os.Getenv("FAKE_CLI_PR_HEAD_SHA")
	if os.Getenv("FAKE_CLI_HEAD_FROM_WORKTREE") != "1" || configured != "deadbeef" {
		return configured
	}
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if realGit == "" {
		fmt.Fprintln(os.Stderr, "missing FAKE_CLI_REAL_GIT")
		os.Exit(1)
	}
	out, err := exec.Command(realGit, "rev-parse", "HEAD").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(out))
}

func fakeGitForward(args []string, realGit string) {
	if realGit == "" {
		fmt.Fprintln(os.Stderr, "missing FAKE_CLI_REAL_GIT")
		os.Exit(1)
	}
	cmd := exec.Command(realGit, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status := exitErr.ExitCode(); status >= 0 {
				os.Exit(status)
			}
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeGlabHandler(args []string) {
	mrViewJSON := os.Getenv("FAKE_CLI_MR_VIEW_JSON")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "list" {
		if mrViewJSON == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		fmt.Println("[" + mrViewJSON + "]")
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "view" {
		if mrViewJSON != "" {
			fmt.Println(mrViewJSON)
			os.Exit(0)
		}
		os.Exit(1)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "update" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "create" {
		fmt.Println("https://gitlab.com/test/repo/-/merge_requests/99")
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "api" && strings.HasSuffix(strings.Join(args, " "), " user") {
		fmt.Println(`{"id":101}`)
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "api" && strings.Contains(strings.Join(args, " "), "/merge_requests/") && strings.Contains(strings.Join(args, " "), "/notes") {
		fakeGlabHandleManagedComments(args)
	}
	os.Exit(1)
}

type fakeGitLabNote struct {
	ID          int64   `json:"id"`
	Body        *string `json:"body"`
	NoteableIID int     `json:"noteable_iid"`
	System      bool    `json:"system"`
	WebURL      string  `json:"web_url"`
	Author      struct {
		ID int64 `json:"id"`
	} `json:"author"`
}

func fakeGlabHandleManagedComments(args []string) {
	path := os.Getenv("FAKE_CLI_PR_COMMENT_FILE")
	if path == "" {
		path = os.Getenv("FAKE_CLI_LOG") + ".comment"
	}
	notes := make([]fakeGitLabNote, 0)
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &notes) != nil {
			fmt.Fprintln(os.Stderr, "invalid fake GitLab note state")
			os.Exit(1)
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	method, _ := fakeCLIFlagValue(args, "--method")
	if method == "" || method == "GET" {
		data, _ := json.Marshal(notes)
		fmt.Println(string(data))
		os.Exit(0)
	}
	payload, err := readFakeBody()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var input struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(payload, &input) != nil {
		fmt.Fprintln(os.Stderr, "invalid fake GitLab note payload")
		os.Exit(1)
	}
	endpoint := ""
	for _, arg := range args {
		if strings.Contains(arg, "/merge_requests/") && strings.Contains(arg, "/notes") {
			endpoint = strings.Split(arg, "?")[0]
			break
		}
	}
	parts := strings.Split(strings.Trim(endpoint, "/"), "/")
	iid := 0
	for i, part := range parts {
		if part == "merge_requests" && i+1 < len(parts) {
			iid, _ = strconv.Atoi(parts[i+1])
		}
	}
	if method == "POST" {
		body := input.Body
		note := fakeGitLabNote{ID: 777, Body: &body, NoteableIID: iid, WebURL: fmt.Sprintf("https://gitlab.com/test/repo/-/merge_requests/%d#note_777", iid)}
		note.Author.ID = 101
		notes = append(notes, note)
		writeFakeGitLabNotes(path, notes)
		data, _ := json.Marshal(note)
		fmt.Println(string(data))
		os.Exit(0)
	}
	if method == "PUT" {
		id, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
		for i := range notes {
			if notes[i].ID == id {
				notes[i].Body = &input.Body
				writeFakeGitLabNotes(path, notes)
				data, _ := json.Marshal(notes[i])
				fmt.Println(string(data))
				os.Exit(0)
			}
		}
	}
	fmt.Fprintln(os.Stderr, "unsupported fake GitLab note operation")
	os.Exit(1)
}

func writeFakeGitLabNotes(path string, notes []fakeGitLabNote) {
	data, _ := json.Marshal(notes)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fakeCLIFlagValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func extractTrailingNumber(rawURL string) int {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return 0
	}
	number, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0
	}
	return number
}

func fakeCIGHReconcileHandler(args []string) {
	joined := strings.Join(args, " ")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	fakeGHHandlePRContentCommands(args, joined)
	if strings.Contains(joined, "pr list") {
		fmt.Println("[]")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr create") {
		fmt.Println("https://github.com/test/repo/pull/42")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		state, err := os.ReadFile(os.Getenv("FAKE_CLI_STATE_PATH"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		switch state := strings.TrimSpace(string(state)); state {
		case "ERROR":
			fmt.Fprintln(os.Stderr, "provider unavailable")
			os.Exit(1)
		default:
			fmt.Println(state)
			os.Exit(0)
		}
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(fakePRHeadSHA())
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		fmt.Println("MERGEABLE")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		fmt.Println(`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		printFakeCommitChecks(`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, args)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "unsupported reconcile gh argv:", joined)
	os.Exit(1)
}

func fakeGHHandlePRContentCommands(args []string, joined string) {
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json title,body") {
		if raw, ok := os.LookupEnv("FAKE_CLI_PR_CONTENT_JSON"); ok {
			fmt.Println(raw)
			os.Exit(0)
		}
		title := os.Getenv("FAKE_CLI_PR_TITLE")
		if title == "" {
			title = "test pr"
		}
		body := os.Getenv("FAKE_CLI_PR_BODY")
		if path, explicit := fakeGHPRBodyPath(); path != "" {
			data, err := os.ReadFile(path)
			if err == nil {
				body = string(data)
			} else if explicit || !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		payload, err := json.Marshal(map[string]string{"title": title, "body": body})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(string(payload))
		os.Exit(0)
	}
	if strings.Contains(joined, "pr edit") {
		if editErr := os.Getenv("FAKE_CLI_PR_EDIT_ERR"); editErr != "" {
			fmt.Fprintln(os.Stderr, editErr)
			os.Exit(1)
		}
		fakeGHStorePRBody(args)
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "api" && strings.HasSuffix(joined, " user") {
		fmt.Println(`{"id":101}`)
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "api" && strings.Contains(joined, "/issues/") && strings.Contains(joined, "comments") {
		fakeGHHandleManagedComments(args)
	}
}

func fakeGHPRBodyPath() (string, bool) {
	if path := os.Getenv("FAKE_CLI_PR_BODY_FILE"); path != "" {
		return path, true
	}
	if logPath := os.Getenv("FAKE_CLI_LOG"); logPath != "" {
		return logPath + ".body", false
	}
	return "", false
}

func fakeGHStorePRBody(args []string) {
	path, _ := fakeGHPRBodyPath()
	bodyFile, _ := fakeCLIFlagValue(args, "--body-file")
	if path == "" || bodyFile != "-" {
		return // A base-only edit must not erase the fake's body either.
	}
	body, err := readFakeBody()
	if err == nil {
		err = os.WriteFile(path, body, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type fakeGHIssueComment struct {
	ID       int64   `json:"id"`
	Body     *string `json:"body"`
	HTMLURL  string  `json:"html_url"`
	IssueURL string  `json:"issue_url"`
	User     struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

func fakeGHHandleManagedComments(args []string) {
	path := os.Getenv("FAKE_CLI_PR_COMMENT_FILE")
	if path == "" {
		if bodyPath, _ := fakeGHPRBodyPath(); bodyPath != "" {
			path = bodyPath + ".comment"
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "fake managed comments require a state file")
		os.Exit(1)
	}
	comments := make([]fakeGHIssueComment, 0)
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &comments) != nil {
			fmt.Fprintln(os.Stderr, "invalid fake comment state")
			os.Exit(1)
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	method, _ := fakeCLIFlagValue(args, "--method")
	joined := strings.Join(args, " ")
	if method == "GET" || method == "" {
		data, _ := json.Marshal(comments)
		fmt.Println(string(data))
		os.Exit(0)
	}
	payload, err := readFakeBody()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var input struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(payload, &input) != nil {
		fmt.Fprintln(os.Stderr, "invalid fake comment payload")
		os.Exit(1)
	}
	if method == "POST" {
		number := extractNumberBefore(joined, "/comments")
		repo := "test/repo"
		for _, arg := range args {
			if strings.HasPrefix(arg, "repos/") && strings.Contains(arg, "/issues/") {
				trimmed := strings.TrimPrefix(arg, "repos/")
				repo, _, _ = strings.Cut(trimmed, "/issues/")
				break
			}
		}
		body := input.Body
		comment := fakeGHIssueComment{ID: 777, Body: &body, HTMLURL: "https://github.com/" + repo + "/pull/" + number + "#issuecomment-777", IssueURL: "https://api.github.com/repos/" + repo + "/issues/" + number}
		comment.User.ID = 101
		comments = append(comments, comment)
		writeFakeComments(path, comments)
		data, _ := json.Marshal(comment)
		fmt.Println(string(data))
		os.Exit(0)
	}
	if method == "PATCH" {
		id := 0
		for _, arg := range args {
			if strings.Contains(arg, "/issues/comments/") {
				id = extractTrailingNumber(arg)
				break
			}
		}
		for i := range comments {
			if comments[i].ID == int64(id) {
				comments[i].Body = &input.Body
				writeFakeComments(path, comments)
				data, _ := json.Marshal(comments[i])
				fmt.Println(string(data))
				os.Exit(0)
			}
		}
		fmt.Fprintln(os.Stderr, "fake comment not found")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "unsupported fake comment method")
	os.Exit(1)
}

func extractNumberBefore(text, suffix string) string {
	before, _, _ := strings.Cut(text, suffix)
	parts := strings.Split(strings.TrimRight(before, "/"), "/")
	if len(parts) == 0 {
		return "0"
	}
	return parts[len(parts)-1]
}

func writeFakeComments(path string, comments []fakeGHIssueComment) {
	data, _ := json.Marshal(comments)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fakeCIGHHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	stateErr := os.Getenv("FAKE_CLI_STATE_ERR")
	checksJSON := os.Getenv("FAKE_CLI_CHECKS")
	checksErr := os.Getenv("FAKE_CLI_CHECKS_ERR")
	mergeable := os.Getenv("FAKE_CLI_MERGEABLE")
	mergeableErr := os.Getenv("FAKE_CLI_MERGEABLE_ERR")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		if authErr := os.Getenv("FAKE_CLI_AUTH_ERR"); authErr != "" {
			fmt.Fprintln(os.Stderr, authErr)
			os.Exit(1)
		}
		os.Exit(0)
	}
	fakeGHHandlePRContentCommands(args, joined)
	if strings.Contains(joined, "pr list") {
		if prListJSON := os.Getenv("FAKE_CLI_PR_LIST_JSON"); prListJSON != "" {
			fmt.Print(prListJSON)
		} else {
			fmt.Println("[]")
		}
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(fakePRHeadSHA())
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		if mergeableErr != "" {
			fmt.Fprintln(os.Stderr, mergeableErr)
			os.Exit(1)
		}
		if mergeable == "" {
			mergeable = "MERGEABLE"
		}
		fmt.Println(mergeable)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		if stateErr != "" {
			fmt.Fprintln(os.Stderr, stateErr)
			os.Exit(1)
		}
		fmt.Println(state)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		if checksErr != "" {
			fmt.Fprintln(os.Stderr, checksErr)
			os.Exit(1)
		}
		fmt.Println(checksJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		if strings.Contains(joined, "reviewThreads") {
			printFakeReviewThreads(os.Getenv("FAKE_CLI_REVIEW_COMMENTS"))
			os.Exit(0)
		}
		if checksErr != "" {
			fmt.Fprintln(os.Stderr, checksErr)
			os.Exit(1)
		}
		printFakeCommitChecks(checksJSON, args)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "actions/runs") {
		printFakeWorkflowRuns()
		os.Exit(0)
	}
	if strings.Contains(joined, "run rerun") {
		fakeCIGHRerun()
	}
	if strings.Contains(joined, "run view") && strings.Contains(joined, "--json jobs") {
		printFakeRunJobs()
		os.Exit(0)
	}
	if strings.Contains(joined, "run view") {
		fmt.Println("error log output")
		os.Exit(0)
	}
	os.Exit(1)
}

// fakeCIGHRerun answers `gh run rerun`, failing when FAKE_CLI_RERUN_ERR asks it
// to so tests can exercise a provider that refuses the request.
func fakeCIGHRerun() {
	if rerunErr := os.Getenv("FAKE_CLI_RERUN_ERR"); rerunErr != "" {
		fmt.Fprintln(os.Stderr, rerunErr)
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeCIGHSequenceHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	checksPath := os.Getenv("FAKE_CLI_CHECKS_PATH")
	indexPath := os.Getenv("FAKE_CLI_CHECKS_INDEX_PATH")
	mergeable := os.Getenv("FAKE_CLI_MERGEABLE")
	mergeableErr := os.Getenv("FAKE_CLI_MERGEABLE_ERR")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	fakeGHHandlePRContentCommands(args, joined)
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		if mergeableErr != "" {
			fmt.Fprintln(os.Stderr, mergeableErr)
			os.Exit(1)
		}
		if mergeable == "" {
			mergeable = "MERGEABLE"
		}
		fmt.Println(mergeable)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println(state)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(fakePRHeadSHA())
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		data, err := os.ReadFile(checksPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		entries := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(entries) == 0 || entries[0] == "" {
			fmt.Println("[]")
			os.Exit(0)
		}

		index := 0
		if rawIndex, err := os.ReadFile(indexPath); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(string(rawIndex))); err == nil {
				index = parsed
			}
		}
		if index >= len(entries) {
			index = len(entries) - 1
		}
		if err := os.WriteFile(indexPath, []byte(strconv.Itoa(index+1)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(entries[index])
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		data, err := os.ReadFile(checksPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		entries := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(entries) == 0 || entries[0] == "" {
			printFakeCommitChecks("[]", args)
			os.Exit(0)
		}
		index := 0
		if rawIndex, err := os.ReadFile(indexPath); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(string(rawIndex))); err == nil {
				index = parsed
			}
		}
		if index >= len(entries) {
			index = len(entries) - 1
		}
		if err := os.WriteFile(indexPath, []byte(strconv.Itoa(index+1)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		printFakeCommitChecks(entries[index], args)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "actions/runs") {
		printFakeWorkflowRuns()
		os.Exit(0)
	}
	if strings.Contains(joined, "run rerun") {
		fakeCIGHRerun()
	}
	if strings.Contains(joined, "run view") && strings.Contains(joined, "--json jobs") {
		printFakeRunJobs()
		os.Exit(0)
	}
	if strings.Contains(joined, "run view") {
		fmt.Println("error log output")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeCIGlabHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	if state == "" {
		state = "opened"
	}
	checksJSON := os.Getenv("FAKE_CLI_CHECKS")
	if checksJSON == "" {
		checksJSON = "[]"
	}
	conflicts := "false"
	if os.Getenv("FAKE_CLI_MR_CONFLICTS") == "true" {
		conflicts = "true"
	}
	mergeStatus := os.Getenv("FAKE_CLI_MERGE_STATUS")
	if mergeStatus == "" {
		mergeStatus = "mergeable"
	}
	traceOutput := os.Getenv("FAKE_CLI_TRACE")
	if traceOutput == "" {
		traceOutput = "gitlab job trace output"
	}
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "mr view") {
		fmt.Printf(`{"iid":42,"web_url":"https://gitlab.com/test/repo/-/merge_requests/42","state":%q,"has_conflicts":%s,"detailed_merge_status":%q,"head_pipeline":{"id":7}}`,
			state, conflicts, mergeStatus)
		fmt.Println()
		os.Exit(0)
	}
	if strings.Contains(joined, "mr create") {
		fmt.Println("https://gitlab.com/test/repo/-/merge_requests/99")
		os.Exit(0)
	}
	if strings.Contains(joined, "mr update") {
		os.Exit(0)
	}
	if strings.Contains(joined, "ci status") {
		fmt.Println(checksJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "ci get") {
		fmt.Println(checksJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "ci trace") {
		fmt.Println(traceOutput)
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeCIGlabSequenceHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	if state == "" {
		state = "opened"
	}
	conflicts := "false"
	if os.Getenv("FAKE_CLI_MR_CONFLICTS") == "true" {
		conflicts = "true"
	}
	mergeStatus := os.Getenv("FAKE_CLI_MERGE_STATUS")
	if mergeStatus == "" {
		mergeStatus = "mergeable"
	}
	checksPath := os.Getenv("FAKE_CLI_CHECKS_PATH")
	indexPath := os.Getenv("FAKE_CLI_CHECKS_INDEX_PATH")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "mr view") {
		fmt.Printf(`{"iid":42,"web_url":"https://gitlab.com/test/repo/-/merge_requests/42","state":%q,"has_conflicts":%s,"detailed_merge_status":%q,"head_pipeline":{"id":7}}`,
			state, conflicts, mergeStatus)
		fmt.Println()
		os.Exit(0)
	}
	if strings.Contains(joined, "ci status") || strings.Contains(joined, "ci get") {
		data, err := os.ReadFile(checksPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		entries := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(entries) == 0 || entries[0] == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		index := 0
		if rawIndex, err := os.ReadFile(indexPath); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(string(rawIndex))); err == nil {
				index = parsed
			}
		}
		if index >= len(entries) {
			index = len(entries) - 1
		}
		if err := os.WriteFile(indexPath, []byte(strconv.Itoa(index+1)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(entries[index])
		os.Exit(0)
	}
	if strings.Contains(joined, "ci trace") {
		fmt.Println("gitlab job trace output")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeCIGHNoChecksHandler(args []string) {
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	fakeGHHandlePRContentCommands(args, joined)
	if strings.Contains(joined, "pr checks") {
		fmt.Fprintln(os.Stderr, "no checks reported on the 'feature/e2e' branch")
		os.Exit(1)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "actions/runs") {
		printFakeWorkflowRuns()
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		printFakeCommitChecks("[]", args)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println("OPEN")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(fakePRHeadSHA())
		os.Exit(0)
	}
	os.Exit(1)
}

// printFakeRunJobs answers `gh run view <id> --json jobs`. The default empty
// job list is what GitHub reports for a workflow run it is holding for
// maintainer approval - it concluded without running anything.
func printFakeRunJobs() {
	raw := os.Getenv("FAKE_CLI_RUN_JOBS")
	if raw == "" {
		raw = "[]"
	}
	fmt.Printf("{\"jobs\":%s}\n", raw)
}

func printFakeWorkflowRuns() {
	raw := os.Getenv("FAKE_CLI_WORKFLOW_RUNS")
	if raw == "" {
		raw = "[]"
	}
	var runs []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &runs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	page := struct {
		TotalCount   int               `json:"total_count"`
		WorkflowRuns []json.RawMessage `json:"workflow_runs"`
	}{
		TotalCount:   len(runs),
		WorkflowRuns: runs,
	}
	encoded, err := json.Marshal([]any{page})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func printFakeCommitChecks(raw string, args []string) {
	var checks []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
		Link        string `json:"link"`
		// App is the check suite's app slug, rendered the way GitHub's
		// GraphQL rollup reports it (checkSuite.app.slug). Empty omits it.
		App string `json:"app"`
	}
	if err := json.Unmarshal([]byte(raw), &checks); err != nil {
		fmt.Println(raw)
		return
	}
	nodes := make([]map[string]any, 0, len(checks))
	for _, check := range checks {
		status := check.Status
		if status == "" {
			status = "COMPLETED"
		}
		conclusion := check.State
		if conclusion == "" {
			conclusion = check.Conclusion
		}
		if conclusion == "" {
			switch check.Bucket {
			case "pass":
				conclusion = "SUCCESS"
			case "fail":
				conclusion = "FAILURE"
			case "cancel":
				conclusion = "CANCELLED"
			case "skip":
				conclusion = "SKIPPED"
			}
		}
		if check.Bucket == "pending" {
			status = "IN_PROGRESS"
			conclusion = ""
		}
		link := check.Link
		if repo := fakeGraphQLRepo(args); repo != "" {
			link = strings.Replace(link, "github.com/test/repo/", "github.com/"+repo+"/", 1)
		}
		node := map[string]any{
			"__typename": "CheckRun", "name": check.Name, "status": status,
			"conclusion": conclusion, "completedAt": check.CompletedAt, "detailsUrl": link,
		}
		if check.App != "" {
			node["checkSuite"] = map[string]any{"app": map[string]any{"slug": check.App}}
		}
		nodes = append(nodes, node)
	}
	response := map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"object": map[string]any{
					"statusCheckRollup": map[string]any{
						"contexts": map[string]any{
							"nodes":    nodes,
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

// printFakeReviewThreads renders FAKE_CLI_REVIEW_COMMENTS - a JSON array of
// {author, path, line, body} - as the reviewThreads GraphQL response the
// GitHub backend's GetReviewComments parses, one unresolved thread per
// comment. An empty or invalid value renders a pull request with no threads.
func printFakeReviewThreads(raw string) {
	var comments []struct {
		Author string `json:"author"`
		Path   string `json:"path"`
		Line   int    `json:"line"`
		Body   string `json:"body"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &comments); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	threads := make([]map[string]any, 0, len(comments))
	for i, comment := range comments {
		threads = append(threads, map[string]any{
			"isResolved": false,
			"comments": map[string]any{
				"nodes": []map[string]any{{
					"databaseId": i + 1,
					"body":       comment.Body,
					"path":       comment.Path,
					"line":       comment.Line,
					"url":        fmt.Sprintf("https://github.com/test/repo/pull/42#discussion_r%d", i+1),
					"createdAt":  "2026-09-07T00:00:00Z",
					"author":     map[string]any{"login": comment.Author},
				}},
			},
		})
	}
	response := map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviewThreads": map[string]any{
						"nodes":    threads,
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func fakeGraphQLRepo(args []string) string {
	var owner, name string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "owner="):
			owner = strings.TrimPrefix(arg, "owner=")
		case strings.HasPrefix(arg, "name="):
			name = strings.TrimPrefix(arg, "name=")
		}
	}
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}
