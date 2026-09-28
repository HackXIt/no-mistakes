package steps

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const (
	validationDetailsOpen  = "<details>\n<summary>Validation</summary>\n\n"
	validationDetailsClose = "\n</details>"
)

func appendixMode(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Config == nil {
		return config.PRAppendixFull
	}
	return sctx.Config.PR.AppendixMode()
}

func wrapValidation(inner string) string {
	inner = strings.Trim(inner, "\n")
	if inner == "" {
		return ""
	}
	return validationDetailsOpen + inner + validationDetailsClose
}

func oneLineRisk(risk string) string {
	return strings.Join(strings.Fields(risk), " ")
}

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
