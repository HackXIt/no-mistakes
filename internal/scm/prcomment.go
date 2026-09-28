package scm

// MaxManagedPRCommentBytes is a conservative cross-provider publication
// budget for the pipeline-owned validation comment. The supported forges all
// accept at least this many UTF-8 bytes for an ordinary PR/MR comment. Keeping
// one shared lower bound makes a rerun render the same evidence after provider
// routing changes and avoids provider-side silent truncation.
const MaxManagedPRCommentBytes = 30 * 1024
