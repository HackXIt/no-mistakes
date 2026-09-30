package scm

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

const maxWindowsCommandLineUTF16 = 32767

func CheckWindowsCommandLine(goos, executable string, args []string) error {
	if goos != "windows" {
		return nil
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, executable)
	parts = append(parts, args...)
	var commandLine strings.Builder
	for i, part := range parts {
		if i > 0 {
			commandLine.WriteByte(' ')
		}
		commandLine.WriteString(escapeWindowsArg(part))
	}
	units := len(utf16.Encode([]rune(commandLine.String()))) + 1
	if units <= maxWindowsCommandLineUTF16 {
		return nil
	}
	return fmt.Errorf("encoded command line uses %d UTF-16 code units including the terminator (Windows limit %d)", units, maxWindowsCommandLineUTF16)
}

func escapeWindowsArg(value string) string {
	if value == "" {
		return `""`
	}
	needsBackslash := false
	hasSpace := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"', '\\':
			needsBackslash = true
		case ' ', '\t':
			hasSpace = true
		}
	}
	if !needsBackslash && !hasSpace {
		return value
	}
	if !needsBackslash {
		return `"` + value + `"`
	}
	var escaped strings.Builder
	if hasSpace {
		escaped.WriteByte('"')
	}
	slashes := 0
	for i := 0; i < len(value); i++ {
		character := value[i]
		switch character {
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				escaped.WriteByte('\\')
			}
			escaped.WriteByte('\\')
		default:
			slashes = 0
		}
		escaped.WriteByte(character)
	}
	if hasSpace {
		for ; slashes > 0; slashes-- {
			escaped.WriteByte('\\')
		}
		escaped.WriteByte('"')
	}
	return escaped.String()
}
