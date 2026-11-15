package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func TestConfigMenuNavigatesAndSavesSelections(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	model := newConfigMenuModel(*cfg)

	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if model.mode != configMenuSection || model.section != 1 {
		t.Fatalf("menu state = mode %d section %d, want Agent section", model.mode, model.section)
	}
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if model.mode != configMenuSelect {
		t.Fatalf("runtime edit mode = %d, want selection menu", model.mode)
	}
	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if model.cfg.Runtime != "docker" {
		t.Fatalf("runtime = %q, want docker", model.cfg.Runtime)
	}

	model = sendConfigMenuKey(t, model, tea.KeyEscape)
	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if model.section != 3 || model.mode != configMenuSection {
		t.Fatalf("menu state = mode %d section %d, want Network section", model.mode, model.section)
	}
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if model.cfg.Network != "proxy" {
		t.Fatalf("network = %q, want proxy", model.cfg.Network)
	}

	model = sendConfigMenuKey(t, model, tea.KeyEscape)
	for model.rootSelection < len(model.sections) {
		model = sendConfigMenuKey(t, model, tea.KeyDown)
	}
	if model.rootSelection != len(model.sections) {
		t.Fatalf("root selection = %d, want save item at %d", model.rootSelection, len(model.sections))
	}
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if !model.saved {
		t.Fatalf("menu did not save valid configuration: %s", model.err)
	}
}

func TestConfigMenuDiscardsOnQuit(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	model := newConfigMenuModel(*cfg)
	model = sendConfigMenuKey(t, model, tea.KeyRunes, 'q')
	if model.saved {
		t.Fatal("quit without saving marked the configuration as saved")
	}
}

func TestConfigMenuPromptsAndSavesOnExitWithChanges(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	model := newConfigMenuModel(*cfg)
	model.cfg.Image = "agentbox:changed"

	model = sendConfigMenuKey(t, model, tea.KeyRunes, 'q')
	if model.mode != configMenuExitConfirm {
		t.Fatalf("exit mode = %d, want unsaved-changes confirmation", model.mode)
	}
	if !strings.Contains(model.View(), "Unsaved changes") {
		t.Fatal("exit confirmation does not mention unsaved changes")
	}
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if !model.saved {
		t.Fatalf("save choice did not save valid configuration: %s", model.err)
	}
}

func TestConfigMenuCanDiscardChangesAfterExitPrompt(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	model := newConfigMenuModel(*cfg)
	model.cfg.Image = "agentbox:changed"

	model = sendConfigMenuKey(t, model, tea.KeyRunes, 'q')
	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if model.saved {
		t.Fatal("discard choice marked the configuration as saved")
	}
	if model.cfg.Image != cfg.Image {
		t.Fatalf("discarded image = %q, want original %q", model.cfg.Image, cfg.Image)
	}
}

func TestConfigMenuPromptsForUnappliedEditorChanges(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	model := newConfigMenuModel(*cfg)
	model.mode = configMenuSection
	updated, _ := model.beginEdit(configMenuField{key: "image", label: "Agent image"})
	model = updated.(configMenuModel)
	model.input.SetValue("agentbox:changed")

	updated, _ = model.requestExit()
	model = updated.(configMenuModel)
	if model.mode != configMenuExitConfirm {
		t.Fatalf("exit mode = %d, want unsaved-changes confirmation", model.mode)
	}
	if model.cfg.Image != "agentbox:changed" {
		t.Fatalf("pending editor value = %q, want it included in save prompt", model.cfg.Image)
	}
}

func TestConfigMenuMasksGitHubToken(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	cfg.GitHubToken = "ghp-do-not-display"
	model := newConfigMenuModel(*cfg)
	field := configMenuField{key: "github_token", label: "GitHub token", secret: true}

	if preview := configMenuFieldPreview(model.cfg, field.key); strings.Contains(preview, cfg.GitHubToken) {
		t.Fatalf("token preview revealed the secret: %q", preview)
	}
	updated, _ := model.beginEdit(field)
	model = updated.(configMenuModel)
	if model.input.EchoMode != textinput.EchoPassword {
		t.Fatalf("token input echo mode = %d, want password mode", model.input.EchoMode)
	}
	if view := model.View(); strings.Contains(view, cfg.GitHubToken) {
		t.Fatalf("token editor revealed the secret: %q", view)
	}
}

func TestConfigMenuSelectsMultipleToolchains(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	model := newConfigMenuModel(*cfg)
	model.section = 2
	model.mode = configMenuSection

	updated, _ := model.beginEdit(model.sections[model.section].fields[0])
	model = updated.(configMenuModel)
	if model.mode != configMenuMultiSelect {
		t.Fatalf("toolchain edit mode = %d, want multi-select", model.mode)
	}
	model = sendConfigMenuKey(t, model, tea.KeyRunes, ' ')
	model = sendConfigMenuKey(t, model, tea.KeyDown)
	model = sendConfigMenuKey(t, model, tea.KeyRunes, ' ')
	model = sendConfigMenuKey(t, model, tea.KeyEnter)

	want := []string{"java", "nodejs"}
	if !reflect.DeepEqual(model.cfg.Toolchains, want) {
		t.Fatalf("selected toolchains = %#v, want %#v", model.cfg.Toolchains, want)
	}
	if fields := model.visibleSectionFields(); len(fields) != 3 {
		t.Fatalf("container fields without custom selection = %d, want 3", len(fields))
	}
}

func TestConfigMenuShowsCustomContainerfileWhenSelected(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	cfg.Toolchains = []string{customToolchain}
	model := newConfigMenuModel(*cfg)
	model.section = 2
	fields := model.visibleSectionFields()
	if len(fields) != 4 || fields[1].key != "custom_containerfile" {
		t.Fatalf("container fields with custom selection = %#v, want custom Containerfile field", fields)
	}
}

func TestConfigMenuCombinesCustomContainerfileWithBuiltinToolchain(t *testing.T) {
	cfg := defaultConfig("/project")
	applyConfigDefaults(cfg)
	model := newConfigMenuModel(*cfg)
	model.section = 2
	model.mode = configMenuSection

	updated, _ := model.beginEdit(model.sections[model.section].fields[0])
	model = updated.(configMenuModel)
	model = sendConfigMenuKey(t, model, tea.KeyRunes, ' ')
	for range 5 {
		model = sendConfigMenuKey(t, model, tea.KeyDown)
	}
	model = sendConfigMenuKey(t, model, tea.KeyRunes, ' ')
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if !reflect.DeepEqual(model.cfg.Toolchains, []string{"java", customToolchain}) {
		t.Fatalf("selected toolchains = %#v, want Java plus custom base", model.cfg.Toolchains)
	}

	model.sectionSelection = 1
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	model.input.SetValue("Containerfile.custom")
	model = sendConfigMenuKey(t, model, tea.KeyEnter)
	if model.cfg.CustomContainerfile != "Containerfile.custom" {
		t.Fatalf("custom Containerfile = %q", model.cfg.CustomContainerfile)
	}
}

func sendConfigMenuKey(t *testing.T, model configMenuModel, key tea.KeyType, runes ...rune) configMenuModel {
	t.Helper()
	updated, _ := model.Update(tea.KeyMsg{Type: key, Runes: runes})
	result, ok := updated.(configMenuModel)
	if !ok {
		t.Fatalf("menu update returned %T, want configMenuModel", updated)
	}
	return result
}
