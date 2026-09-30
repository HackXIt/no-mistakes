package scm

import (
	"strings"
	"testing"
)

func TestCheckWindowsCommandLineBoundary(t *testing.T) {
	t.Parallel()
	if err := CheckWindowsCommandLine("windows", "x", []string{strings.Repeat("a", maxWindowsCommandLineUTF16-3)}); err != nil {
		t.Fatalf("command at Windows limit: %v", err)
	}
	if err := CheckWindowsCommandLine("windows", "x", []string{strings.Repeat("a", maxWindowsCommandLineUTF16-2)}); err == nil {
		t.Fatal("command beyond Windows limit was accepted")
	}
}

func TestCheckWindowsCommandLineAccountsForEscapingAndPlatform(t *testing.T) {
	t.Parallel()
	quoteHeavy := strings.Repeat(`"`, maxWindowsCommandLineUTF16/2)
	if err := CheckWindowsCommandLine("windows", "x", []string{quoteHeavy}); err == nil {
		t.Fatal("quote-heavy Windows command was accepted")
	}
	if err := CheckWindowsCommandLine("windows", "tea", []string{"api", "--field", "body=validation body"}); err != nil {
		t.Fatalf("normal Windows command: %v", err)
	}
	if err := CheckWindowsCommandLine("linux", "x", []string{strings.Repeat(`"`, maxWindowsCommandLineUTF16)}); err != nil {
		t.Fatalf("non-Windows command was refused: %v", err)
	}
}
