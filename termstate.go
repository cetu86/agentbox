package main

import (
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// logScanTimeout bounds how long recovering the terminal state from the
// container log may delay an attach.
const logScanTimeout = 5 * time.Second

// restoredModes are the DEC private modes a full-screen application sets once
// at startup and never repeats, so a late attach has to learn them elsewhere.
var restoredModes = []int{1049, 9, 1000, 1002, 1003, 1006, 2004, 25}

// modeScanner follows DEC private mode changes (CSI ? Pm h / l) in a stream
// of terminal output.
type modeScanner struct {
	state map[int]bool
	carry []byte
}

func newModeScanner() *modeScanner {
	return &modeScanner{state: map[int]bool{}}
}

func (s *modeScanner) Write(p []byte) (int, error) {
	data := append(s.carry, p...)
	s.carry = nil
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			i++
			continue
		}
		if i+1 >= len(data) {
			s.carry = append([]byte(nil), data[i:]...)
			break
		}
		if data[i+1] != '[' {
			i++
			continue
		}
		j := i + 2
		if j >= len(data) {
			s.carry = append([]byte(nil), data[i:]...)
			break
		}
		if data[j] != '?' {
			i = j
			continue
		}
		j++
		for j < len(data) && (data[j] >= '0' && data[j] <= '9' || data[j] == ';') {
			j++
		}
		if j >= len(data) {
			if len(data)-i <= 64 {
				s.carry = append([]byte(nil), data[i:]...)
			}
			break
		}
		if final := data[j]; final == 'h' || final == 'l' {
			for _, param := range strings.Split(string(data[i+3:j]), ";") {
				if mode, err := strconv.Atoi(param); err == nil {
					s.state[mode] = final == 'h'
				}
			}
		}
		i = j + 1
	}
	return len(p), nil
}

// sequence rebuilds the sequences that put a fresh terminal into the state the
// stream left it in. The alternate screen comes first, like at startup.
func (s *modeScanner) sequence() string {
	var b strings.Builder
	for _, mode := range restoredModes {
		on, seen := s.state[mode]
		if !seen || (!on && mode != 25) {
			continue
		}
		b.WriteString("\x1b[?" + strconv.Itoa(mode))
		if on {
			b.WriteString("h")
		} else {
			b.WriteString("l")
		}
	}
	return b.String()
}

// terminalState reads the container log and returns the sequences that
// restore the modes the application enabled before agentbox attached. The
// runtime does not replay them on attach, so without this a reattached view
// would not know that the application wants mouse input. It returns an empty
// string when the log is unavailable.
func (c *Config) terminalState(container string) string {
	ctx, cancel := context.WithTimeout(context.Background(), logScanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Runtime, "logs", container)
	scanner := newModeScanner()
	cmd.Stdout = scanner
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return ""
	}
	return scanner.sequence()
}
