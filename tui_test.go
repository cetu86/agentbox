package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func testTUIModel(boxes ...boxInfo) tuiModel {
	m := newTUIModel(&Config{Project: "/work/proj", Runtime: "podman"})
	m.boxes, m.loaded = boxes, true
	return m
}

func press(t *testing.T, m tuiModel, keys ...string) (tuiModel, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		next, c := m.Update(msg)
		m, cmd = next.(tuiModel), c
	}
	return m, cmd
}

func TestSuggestNameSkipsTakenNames(t *testing.T) {
	m := testTUIModel(boxInfo{Name: "agent-1"}, boxInfo{Name: "agent-2"}, boxInfo{Name: "agent-4"})
	if got := m.suggestName(); got != "agent-3" {
		t.Fatalf("suggestName() = %q, want agent-3", got)
	}
}

func TestNewBoxValidatesName(t *testing.T) {
	m := testTUIModel(boxInfo{Name: "taken"})
	for _, name := range []string{"", "Bad Name", "taken"} {
		m = m.openNew()
		m.nameInput.SetValue(name)
		m, _ = press(t, m, "enter", "enter")
		if !m.statusErr || m.screen != screenNew {
			t.Fatalf("name %q: statusErr=%v screen=%v; want an error and the form kept", name, m.statusErr, m.screen)
		}
	}
	m = m.openNew()
	m.nameInput.SetValue("fresh")
	m, cmd := press(t, m, "enter", "enter")
	if cmd == nil || m.busy == "" || m.screen != screenList {
		t.Fatalf("valid name should start an operation: busy=%q screen=%v", m.busy, m.screen)
	}
}

func TestListNavigationAndEmptyList(t *testing.T) {
	m := testTUIModel()
	m, _ = press(t, m, "j", "k", "G", "g")
	if m.cursor != 0 {
		t.Fatalf("cursor = %d on an empty list", m.cursor)
	}
	m, cmd := press(t, m, "a")
	if cmd != nil || !m.statusErr {
		t.Fatal("acting without a box should show an error")
	}
	if view := ansi.Strip(m.View()); !strings.Contains(view, "Press n to start") {
		t.Fatalf("empty list view lacks a hint:\n%s", view)
	}

	m = testTUIModel(boxInfo{Name: "a"}, boxInfo{Name: "b"}, boxInfo{Name: "c"})
	m, _ = press(t, m, "j", "j", "j")
	if m.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.cursor)
	}
	m, _ = press(t, m, "g")
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.cursor)
	}
}

func TestAcceptRefusedWhileRunning(t *testing.T) {
	m := testTUIModel(boxInfo{Name: "a", Running: true, Status: "running"})
	for _, key := range []string{"a", "m", "p"} {
		next, cmd := press(t, m, key)
		if cmd != nil || next.screen != screenList || !next.statusErr {
			t.Fatalf("key %s on a running box: screen=%v err=%v", key, next.screen, next.statusErr)
		}
	}
}

func TestConfirmFlow(t *testing.T) {
	m := testTUIModel(boxInfo{Name: "a", Branch: "agent/a"})
	m, _ = press(t, m, "d")
	if m.screen != screenConfirm || !m.confirm.danger {
		t.Fatalf("discard should open a danger confirmation, screen=%v", m.screen)
	}
	m, cmd := press(t, m, "n")
	if m.screen != screenList || cmd != nil {
		t.Fatal("n should cancel")
	}
	m, _ = press(t, m, "m")
	if m.screen != screenConfirm || !strings.Contains(strings.Join(m.confirm.lines, " "), "git merge") {
		t.Fatalf("merge confirmation should mention git merge: %+v", m.confirm)
	}
	m, cmd = press(t, m, "y")
	if m.screen != screenList || m.busy == "" || cmd == nil {
		t.Fatalf("y should start the operation: busy=%q", m.busy)
	}
	// While busy, keys are ignored.
	m2, cmd2 := press(t, m, "d")
	if m2.screen != screenList || cmd2 != nil {
		t.Fatal("keys must be ignored while an operation is running")
	}
}

func TestOpDoneReportsErrorsWithLog(t *testing.T) {
	m := testTUIModel(boxInfo{Name: "a"})
	m.busy = "working"
	next, _ := m.Update(opDoneMsg{err: errors.New("boom"), log: "+ git x\nfatal: nope\n"})
	m = next.(tuiModel)
	if m.busy != "" || !m.statusErr || !strings.Contains(m.status, "boom") || !strings.Contains(m.status, "fatal: nope") {
		t.Fatalf("status = %q busy=%q", m.status, m.busy)
	}
	next, _ = m.Update(opDoneMsg{message: "done"})
	m = next.(tuiModel)
	if m.statusErr || m.status != "done" {
		t.Fatalf("status = %q err=%v", m.status, m.statusErr)
	}
}

func TestBoxRefreshKeepsSelectionByName(t *testing.T) {
	m := testTUIModel(boxInfo{Name: "a"}, boxInfo{Name: "b"}, boxInfo{Name: "c"})
	m.cursor = 2
	m = m.applyBoxes(boxesMsg{boxes: []boxInfo{{Name: "b"}, {Name: "c"}}})
	if b, _ := m.selected(); b.Name != "c" {
		t.Fatalf("selected %q, want c", b.Name)
	}
	m = m.applyBoxes(boxesMsg{boxes: nil})
	if m.cursor != 0 {
		t.Fatalf("cursor = %d on empty list", m.cursor)
	}
}

func TestViewFitsTerminal(t *testing.T) {
	m := testTUIModel(
		boxInfo{Name: "alpha", Branch: "agent/alpha", Running: true, Status: "running", Dirty: true},
		boxInfo{Name: "beta", Branch: "agent/beta", Status: "exited"},
	)
	m.width, m.height = 60, 15
	for _, screen := range []tuiScreen{screenList, screenConfirm, screenNew} {
		m.screen = screen
		m.confirm = confirmDialog{title: "t", lines: []string{"l"}}
		view := m.View()
		lines := strings.Split(view, "\n")
		if screen == screenList && len(lines) != m.height {
			t.Fatalf("list view has %d lines, want %d", len(lines), m.height)
		}
		for _, line := range lines {
			if w := ansi.StringWidth(line); w > m.width {
				t.Fatalf("screen %v: line width %d > %d: %q", screen, w, m.width, line)
			}
		}
	}
}

func TestFitAndWrapKeys(t *testing.T) {
	if got := fit("abcdef", 4); ansi.StringWidth(got) != 4 {
		t.Fatalf("fit truncate = %q", got)
	}
	if got := fit("ab", 4); got != "ab  " {
		t.Fatalf("fit pad = %q", got)
	}
	lines := wrapKeys([]string{"aaaa", "bbbb", "cccc"}, 10)
	if len(lines) != 2 || lines[0] != "aaaa  bbbb" || lines[1] != "cccc" {
		t.Fatalf("wrapKeys = %q", lines)
	}
}

func TestColorizeDiff(t *testing.T) {
	out := colorizeDiff("+add\n-del\n@@ x\n ctx")
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], sgrTUIGreen) || !strings.Contains(lines[1], sgrTUIRed) ||
		!strings.Contains(lines[2], sgrTUICyan) || lines[3] != " ctx" {
		t.Fatalf("colorizeDiff = %q", out)
	}
}

func TestLastLinesSkipsEchoedCommands(t *testing.T) {
	if got := lastLines("+ git a\nfirst\n\nsecond\n+ git b\n", 2); got != "first | second" {
		t.Fatalf("lastLines = %q", got)
	}
}
