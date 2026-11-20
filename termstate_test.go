package main

import (
	"os"
	"path/filepath"
	"testing"
)

func scanModes(t *testing.T, chunks ...string) string {
	t.Helper()
	s := newModeScanner()
	for _, c := range chunks {
		if _, err := s.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	return s.sequence()
}

func TestModeScannerStartup(t *testing.T) {
	got := scanModes(t, "\x1b[?1049h\x1b[?2004h\x1b[?1004h\x1b[?1003h\x1b[?1006h\x1b[?25l text")
	want := "\x1b[?1049h\x1b[?1003h\x1b[?1006h\x1b[?2004h\x1b[?25l"
	if got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
}

func TestModeScannerLastValueWins(t *testing.T) {
	got := scanModes(t, "\x1b[?1000h\x1b[?1003h", "\x1b[?1003l\x1b[?25l\x1b[?25h")
	if want := "\x1b[?1000h\x1b[?25h"; got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
}

func TestModeScannerMultipleParams(t *testing.T) {
	got := scanModes(t, "\x1b[?1002;1006h")
	if want := "\x1b[?1002h\x1b[?1006h"; got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
}

func TestModeScannerSplitSequences(t *testing.T) {
	full := "abc\x1b[?1049h\x1b[?1003;1006h"
	for i := 1; i < len(full); i++ {
		got := scanModes(t, full[:i], full[i:])
		if want := "\x1b[?1049h\x1b[?1003h\x1b[?1006h"; got != want {
			t.Fatalf("split at %d: sequence = %q, want %q", i, got, want)
		}
	}
}

func TestModeScannerIgnoresOtherSequences(t *testing.T) {
	got := scanModes(t, "\x1b[2J\x1b[1;5H\x1b[?7l\x1b[31m\x1b]0;title\a\x1b[?1004h\x1b")
	if got != "" {
		t.Fatalf("sequence = %q, want none", got)
	}
}

func TestTerminalStateReadsLog(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = logs ] && printf '\\033[?1049h\\033[?1003h\\033[?1006h'\n"
	runtime := filepath.Join(dir, "fakert")
	if err := os.WriteFile(runtime, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Config{Runtime: runtime}
	if got, want := c.terminalState("box"), "\x1b[?1049h\x1b[?1003h\x1b[?1006h"; got != want {
		t.Fatalf("terminalState = %q, want %q", got, want)
	}
	c.Runtime = filepath.Join(dir, "missing")
	if got := c.terminalState("box"); got != "" {
		t.Fatalf("terminalState with failing runtime = %q, want empty", got)
	}
}
