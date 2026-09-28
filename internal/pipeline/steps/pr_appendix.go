package steps

import "strings"

func extractPipelineAttestationMarker(text string) string {
	start := strings.Index(text, pipelineAttestationCommentPrefix)
	if start < 0 {
		return ""
	}
	rest := text[start:]
	end := strings.Index(rest, pipelineAttestationCommentClosingToken)
	if end < 0 {
		return ""
	}
	return rest[:end+len(pipelineAttestationCommentClosingToken)]
}

func joinBlocks(parts ...string) string {
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n\n")
}
