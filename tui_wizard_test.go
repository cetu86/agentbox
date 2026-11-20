package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func okFacts() projectFacts {
	return projectFacts{Dir: "/p", IsRepo: true, HasCommits: true, Btrfs: true, Subvolume: true, DaemonOK: true, Runtime: "podman"}
}

func actionsOf(options []wizardOption) []wizardAction {
	var out []wizardAction
	for _, o := range options {
		out = append(out, o.action)
	}
	return out
}

func hasAction(options []wizardOption, want wizardAction) bool {
	for _, o := range options {
		if o.action == want {
			return true
		}
	}
	return false
}

func TestWizardChoices(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*projectFacts)
		wantProblem  bool
		wantNote     bool
		wantAction   wizardAction
		wantNoAction wizardAction
	}{
		{"ready", func(*projectFacts) {}, false, false, actContinue, actConvert},
		{"no git", func(f *projectFacts) { f.IsRepo, f.HasCommits = false, false }, true, false, actGitInit, actContinue},
		{"no commits", func(f *projectFacts) { f.HasCommits = false }, true, false, actRecheck, actContinue},
		{"no runtime", func(f *projectFacts) { f.Runtime = "" }, true, false, actRecheck, actContinue},
		{"no btrfs", func(f *projectFacts) { f.Btrfs, f.Subvolume = false, false }, true, false, actRecheck, actConvert},
		{"convert", func(f *projectFacts) { f.Subvolume = false }, false, true, actConvert, actContinue},
		{"convert needs daemon", func(f *projectFacts) { f.Subvolume, f.DaemonOK = false, false }, true, true, actRecheck, actConvert},
		{"subvolume without daemon warns", func(f *projectFacts) { f.DaemonOK = false }, false, true, actContinue, actConvert},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := okFacts()
			tt.mutate(&f)
			problems, notes, options := wizardChoices(f)
			if (len(problems) > 0) != tt.wantProblem || (len(notes) > 0) != tt.wantNote {
				t.Fatalf("problems=%v notes=%v", problems, notes)
			}
			if !hasAction(options, tt.wantAction) || hasAction(options, tt.wantNoAction) || !hasAction(options, actQuit) {
				t.Fatalf("options = %v", actionsOf(options))
			}
		})
	}
}

func TestProbeInspect(t *testing.T) {
	probe := projectProbe{
		gitRoot:    func(string) (string, error) { return "/repo", nil },
		hasCommits: func(string) bool { return true },
		subvolume:  func(dir string) (bool, error) { return dir == "/repo", nil },
		daemon:     func() error { return errors.New("down") },
		lookPath: func(name string) (string, error) {
			if name == "docker" {
				return "/usr/bin/docker", nil
			}
			return "", errors.New("missing")
		},
	}
	f := probe.inspect("/repo/sub")
	want := projectFacts{Dir: "/repo", IsRepo: true, HasCommits: true, Btrfs: true, Subvolume: true, DaemonOK: false, Runtime: "docker"}
	if f != want {
		t.Fatalf("inspect = %+v, want %+v", f, want)
	}

	probe.gitRoot = func(string) (string, error) { return "", errors.New("not a repo") }
	probe.subvolume = func(string) (bool, error) { return false, errors.New("not btrfs") }
	f = probe.inspect("/plain")
	if f.IsRepo || f.Btrfs || f.Dir != "/plain" {
		t.Fatalf("inspect = %+v", f)
	}
}

func wizardModel(f projectFacts) tuiModel {
	m := newTUIModel(nil)
	m.screen = screenWizard
	m.wizard = newWizard(f.Dir)
	m.wizard.applyFacts(f)
	return m
}

func TestWizardFlow(t *testing.T) {
	f := okFacts()
	f.Subvolume = false
	m := wizardModel(f)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	if m.wizard.step != wizardConfig || !m.wizard.convert {
		t.Fatalf("step=%v convert=%v; want config step with conversion", m.wizard.step, m.wizard.convert)
	}
	if view := ansi.Strip(m.viewWizard()); !strings.Contains(view, "converted to a btrfs subvolume") {
		t.Fatalf("summary should mention the conversion:\n%s", view)
	}

	// Initialization failure returns to the config step with the error.
	next, _ = m.Update(wizardInitMsg{err: errors.New("boom"), log: "fatal: x"})
	m = next.(tuiModel)
	if m.wizard.step != wizardConfig || !strings.Contains(m.wizard.err, "boom") {
		t.Fatalf("step=%v err=%q", m.wizard.step, m.wizard.err)
	}

	// A missing image offers a build; skipping lands on the empty list.
	next, _ = m.Update(wizardImageMsg{cfg: defaultConfig("/p"), exists: false})
	m = next.(tuiModel)
	if m.wizard.step != wizardImage || !hasAction(m.wizard.options, actBuildImage) {
		t.Fatalf("step=%v options=%v", m.wizard.step, actionsOf(m.wizard.options))
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(tuiModel)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	if m.screen != screenList || m.cfg == nil || cmd == nil || !strings.Contains(m.status, "first box") {
		t.Fatalf("screen=%v cfg=%v status=%q", m.screen, m.cfg, m.status)
	}
}

func TestWizardExistingImageGoesStraightToList(t *testing.T) {
	m := wizardModel(okFacts())
	next, _ := m.Update(wizardImageMsg{cfg: defaultConfig("/p"), exists: true})
	m = next.(tuiModel)
	if m.screen != screenList || m.cfg == nil {
		t.Fatalf("screen=%v", m.screen)
	}
}

func TestWizardQuitAndBusyKeys(t *testing.T) {
	m := wizardModel(okFacts())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should quit")
	}
	m.wizard.step = wizardWorking
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("keys must be ignored while working")
	}
}

func TestWizardViewFits(t *testing.T) {
	f := okFacts()
	f.IsRepo, f.HasCommits = false, false
	m := wizardModel(f)
	m.width, m.height = 50, 20
	for _, line := range strings.Split(m.viewWizard(), "\n") {
		if w := ansi.StringWidth(line); w > m.width {
			t.Fatalf("line width %d > %d: %q", w, m.width, line)
		}
	}
	if view := ansi.Strip(m.viewWizard()); !strings.Contains(view, "git init") {
		t.Fatalf("view should offer git init:\n%s", view)
	}
}
