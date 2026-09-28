# no-mistakes delivery record

- Task: Publish concise squash-commit-style MR/PR descriptions and maintain detailed validation/evidence in one idempotently updated pipeline-owned comment, while retaining the body-based head attestation contract.
- Branch: `fm/nm-concise-pr-publication`
- Checks: `make lint`; `GIT_CONFIG_GLOBAL=<test copy without worktree.useRelativePaths> go test -race ./...`; targeted e2e publication journeys for concise description/comment split, rerun deduplication, review-conversation publication, and Gitea; `make docs-build`; `go build -o ./bin/no-mistakes ./cmd/no-mistakes`. A full `make e2e` attempt hit unrelated suite-wide active-run timing failures and a live-profile timeout; every affected journey passes in the targeted run. Normal no-mistakes delivery is pending.
- MR/PR URL: pending creation by the no-mistakes delivery pipeline.

This branch was created from the current remote default branch in the dedicated isolated worktree. The retained `fm/nm-review-status-visibility-p0` and `fm/nm-gitlab-host-alias` branches were not reused.
