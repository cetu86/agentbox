package main

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type configMenuMode int

const (
	configMenuRoot configMenuMode = iota
	configMenuSection
	configMenuSelect
	configMenuMultiSelect
	configMenuInput
	configMenuText
	configMenuExitConfirm
)

type configMenuOption struct {
	label string
	value string
}

type configMenuField struct {
	key         string
	label       string
	multiline   bool
	secret      bool
	multiselect bool
	options     []configMenuOption
}

type configMenuCategory struct {
	title  string
	fields []configMenuField
}

type configMenuModel struct {
	cfg              Config
	original         Config
	sections         []configMenuCategory
	mode             configMenuMode
	rootSelection    int
	section          int
	sectionSelection int
	field            configMenuField
	optionSelection  int
	selectedOptions  []bool
	exitSelection    int
	originalValue    string
	input            textinput.Model
	text             textarea.Model
	width            int
	height           int
	err              string
	saved            bool
}

func runConfigMenu(cfg Config) (Config, bool, error) {
	model := newConfigMenuModel(cfg)
	program := tea.NewProgram(model, tea.WithAltScreen())
	finalModel, err := program.Run()
	if err != nil {
		return cfg, false, err
	}
	final, ok := finalModel.(configMenuModel)
	if !ok {
		return cfg, false, fmt.Errorf("unexpected configuration menu result %T", finalModel)
	}
	return final.cfg, final.saved, nil
}

func newConfigMenuModel(cfg Config) configMenuModel {
	cfg = cloneConfig(cfg)
	return configMenuModel{
		cfg:      cfg,
		original: cloneConfig(cfg),
		sections: []configMenuCategory{
			{
				title: "Project",
				fields: []configMenuField{
					{key: "snapshot_dir", label: "Snapshot directory"},
				},
			},
			{
				title: "Agent",
				fields: []configMenuField{
					{
						key:   "runtime",
						label: "Container runtime",
						options: []configMenuOption{
							{label: "Podman", value: "podman"},
							{label: "Docker", value: "docker"},
						},
					},
					{key: "image", label: "Agent image"},
					{key: "agent_cmd", label: "Agent command arguments", multiline: true},
					{key: "shm_size", label: "Shared memory size"},
					{key: "max_parallel", label: "Plan: max parallel agents"},
				},
			},
			{
				title: "Container",
				fields: []configMenuField{
					{key: "toolchains", label: "Language toolchains", multiselect: true, options: toolchainMenuOptions()},
					{key: "custom_containerfile", label: "Custom base Containerfile"},
					{key: "mounts", label: "Extra mounts", multiline: true},
					{key: "env", label: "Environment variables", multiline: true},
				},
			},
			{
				title: "Network",
				fields: []configMenuField{
					{
						key:   "network",
						label: "Network mode",
						options: []configMenuOption{
							{label: "Open (normal container networking)", value: "open"},
							{label: "None (no container networking)", value: "none"},
							{label: "Proxy (domain allowlist)", value: "proxy"},
						},
					},
					{key: "allowed_domains", label: "Allowed domains", multiline: true},
					{key: "proxy_image", label: "Proxy image"},
					{key: "egress_network", label: "Proxy egress network"},
				},
			},
			{
				title: "Credentials",
				fields: []configMenuField{
					{key: "github_token", label: "GitHub token for Copilot", secret: true},
				},
			},
		},
		width:  80,
		height: 24,
	}
}

func (m configMenuModel) Init() tea.Cmd {
	return nil
}

func (m configMenuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.mode == configMenuText {
			m.resizeTextArea()
		}
		return m, nil
	case tea.KeyMsg:
		return m.updateKey(msg)
	default:
		return m, nil
	}
}

func (m configMenuModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		if m.mode == configMenuExitConfirm {
			m.mode = configMenuRoot
			return m, nil
		}
		return m.requestExit()
	}

	switch m.mode {
	case configMenuRoot:
		return m.updateRoot(key)
	case configMenuSection:
		return m.updateSection(key)
	case configMenuSelect:
		return m.updateSelect(key)
	case configMenuMultiSelect:
		return m.updateMultiSelect(key)
	case configMenuInput:
		return m.updateInput(msg, key)
	case configMenuText:
		return m.updateText(msg, key)
	case configMenuExitConfirm:
		return m.updateExitConfirm(key)
	default:
		return m, nil
	}
}

func (m configMenuModel) updateRoot(key string) (tea.Model, tea.Cmd) {
	rootItems := len(m.sections) + 2
	switch key {
	case "up", "k":
		m.rootSelection = (m.rootSelection - 1 + rootItems) % rootItems
	case "down", "j":
		m.rootSelection = (m.rootSelection + 1) % rootItems
	case "enter":
		switch {
		case m.rootSelection < len(m.sections):
			m.section = m.rootSelection
			m.sectionSelection = 0
			m.mode = configMenuSection
		case m.rootSelection == len(m.sections):
			return m.saveAndExit()
		default:
			return m.requestExit()
		}
	case "s", "ctrl+s":
		return m.saveAndExit()
	case "q", "esc":
		return m.requestExit()
	}
	return m, nil
}

func (m configMenuModel) updateSection(key string) (tea.Model, tea.Cmd) {
	fields := m.visibleSectionFields()
	itemCount := len(fields) + 1
	switch key {
	case "up", "k":
		m.sectionSelection = (m.sectionSelection - 1 + itemCount) % itemCount
	case "down", "j":
		m.sectionSelection = (m.sectionSelection + 1) % itemCount
	case "enter":
		if m.sectionSelection == len(fields) {
			m.mode = configMenuRoot
			return m, nil
		}
		return m.beginEdit(fields[m.sectionSelection])
	case "esc":
		m.mode = configMenuRoot
	case "s", "ctrl+s":
		return m.saveAndExit()
	case "q":
		return m.requestExit()
	}
	return m, nil
}

func (m configMenuModel) updateSelect(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k", "left":
		m.optionSelection = (m.optionSelection - 1 + len(m.field.options)) % len(m.field.options)
	case "down", "j", "right":
		m.optionSelection = (m.optionSelection + 1) % len(m.field.options)
	case "enter":
		setConfigMenuField(&m.cfg, m.field.key, m.field.options[m.optionSelection].value)
		m.mode = configMenuSection
		m.err = ""
	case "esc":
		m.mode = configMenuSection
	case "s", "ctrl+s":
		return m.saveAndExit()
	case "q":
		return m.requestExit()
	}
	return m, nil
}

func (m configMenuModel) updateMultiSelect(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		m.optionSelection = (m.optionSelection - 1 + len(m.field.options)) % len(m.field.options)
	case "down", "j":
		m.optionSelection = (m.optionSelection + 1) % len(m.field.options)
	case " ", "space":
		if m.optionSelection >= 0 && m.optionSelection < len(m.field.options) {
			m.selectedOptions = slices.Clone(m.selectedOptions)
			m.selectedOptions[m.optionSelection] = !m.selectedOptions[m.optionSelection]
		}
	case "enter":
		m.applySelectedOptions()
		m.mode = configMenuSection
		m.err = ""
	case "esc":
		m.mode = configMenuSection
	case "s", "ctrl+s":
		m.applySelectedOptions()
		return m.saveAndExit()
	case "q":
		return m.requestExit()
	}
	return m, nil
}

func (m configMenuModel) updateExitConfirm(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		m.exitSelection = (m.exitSelection - 1 + 3) % 3
	case "down", "j":
		m.exitSelection = (m.exitSelection + 1) % 3
	case "enter":
		switch m.exitSelection {
		case 0:
			return m.saveAndExit()
		case 1:
			m.cfg = cloneConfig(m.original)
			m.saved = false
			return m, tea.Quit
		default:
			m.mode = configMenuRoot
			m.err = ""
		}
	case "s":
		return m.saveAndExit()
	case "q", "esc":
		m.mode = configMenuRoot
		m.err = ""
	}
	return m, nil
}

func (m configMenuModel) updateInput(msg tea.Msg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "enter", "ctrl+s":
		setConfigMenuField(&m.cfg, m.field.key, m.input.Value())
		m.mode = configMenuSection
		m.err = ""
		return m, nil
	case "esc":
		m.mode = configMenuSection
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m configMenuModel) updateText(msg tea.Msg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "ctrl+s", "ctrl+enter":
		setConfigMenuField(&m.cfg, m.field.key, m.text.Value())
		m.mode = configMenuSection
		m.err = ""
		return m, nil
	case "esc":
		m.mode = configMenuSection
		return m, nil
	}
	var cmd tea.Cmd
	m.text, cmd = m.text.Update(msg)
	return m, cmd
}

func (m configMenuModel) beginEdit(field configMenuField) (tea.Model, tea.Cmd) {
	m.field = field
	m.originalValue = configMenuFieldText(m.cfg, field.key)
	m.err = ""
	if len(field.options) > 0 && !field.multiselect {
		m.mode = configMenuSelect
		m.optionSelection = 0
		for i, option := range field.options {
			if option.value == m.originalValue {
				m.optionSelection = i
				break
			}
		}
		return m, nil
	}
	if field.multiline {
		m.mode = configMenuText
		m.text = textarea.New()
		m.text.ShowLineNumbers = false
		m.text.SetValue(m.originalValue)
		m.text.Focus()
		m.resizeTextArea()
		return m, nil
	}
	if field.multiselect {
		m.mode = configMenuMultiSelect
		m.optionSelection = 0
		m.selectedOptions = make([]bool, len(field.options))
		selected := make(map[string]bool, len(field.options))
		for _, value := range configTextLines(configMenuFieldText(m.cfg, field.key)) {
			selected[value] = true
		}
		for i, option := range field.options {
			m.selectedOptions[i] = selected[option.value]
		}
		return m, nil
	}
	m.mode = configMenuInput
	m.input = textinput.New()
	m.input.SetValue(m.originalValue)
	if field.secret {
		m.input.EchoMode = textinput.EchoPassword
		m.input.EchoCharacter = '*'
	}
	m.input.Focus()
	return m, textinput.Blink
}

func (m *configMenuModel) resizeTextArea() {
	width := m.width - 4
	if width < 20 {
		width = 20
	}
	height := m.height - 9
	if height < 4 {
		height = 4
	}
	if height > 14 {
		height = 14
	}
	m.text.SetWidth(width)
	m.text.SetHeight(height)
}

func (m configMenuModel) visibleSectionFields() []configMenuField {
	fields := m.sections[m.section].fields
	if slices.Contains(m.cfg.Toolchains, customToolchain) {
		return fields
	}
	visible := make([]configMenuField, 0, len(fields))
	for _, field := range fields {
		if field.key != "custom_containerfile" {
			visible = append(visible, field)
		}
	}
	return visible
}

func (m *configMenuModel) applySelectedOptions() {
	values := make([]string, 0, len(m.selectedOptions))
	for i, selected := range m.selectedOptions {
		if selected && i < len(m.field.options) {
			values = append(values, m.field.options[i].value)
		}
	}
	setConfigMenuField(&m.cfg, m.field.key, strings.Join(values, "\n"))
}

func (m configMenuModel) saveAndExit() (tea.Model, tea.Cmd) {
	if err := validateConfiguredValues(&m.cfg); err != nil {
		m.err = err.Error()
		if m.mode == configMenuExitConfirm {
			m.exitSelection = 0
		}
		return m, nil
	}
	m.saved = true
	return m, tea.Quit
}

func (m configMenuModel) requestExit() (tea.Model, tea.Cmd) {
	switch m.mode {
	case configMenuSelect:
		if m.optionSelection >= 0 && m.optionSelection < len(m.field.options) {
			setConfigMenuField(&m.cfg, m.field.key, m.field.options[m.optionSelection].value)
		}
	case configMenuMultiSelect:
		m.applySelectedOptions()
	case configMenuInput:
		setConfigMenuField(&m.cfg, m.field.key, m.input.Value())
	case configMenuText:
		setConfigMenuField(&m.cfg, m.field.key, m.text.Value())
	}
	if !reflect.DeepEqual(m.cfg, m.original) {
		m.mode = configMenuExitConfirm
		m.exitSelection = 0
		m.err = ""
		return m, nil
	}
	m.saved = false
	return m, tea.Quit
}

func (m configMenuModel) View() string {
	var b strings.Builder
	fmt.Fprintln(&b, "agentbox - Configure")
	fmt.Fprintf(&b, "Project: %s\n", clipConfigText(m.cfg.Project, m.width-10))
	fmt.Fprintln(&b, strings.Repeat("-", positiveWidth(m.width)))

	switch m.mode {
	case configMenuRoot:
		fmt.Fprintln(&b, "Configuration")
		fmt.Fprintln(&b)
		for i, section := range m.sections {
			m.writeMenuItem(&b, i == m.rootSelection, section.title, "Open menu")
		}
		fmt.Fprintln(&b)
		m.writeMenuItem(&b, m.rootSelection == len(m.sections), "Save and exit", "")
		m.writeMenuItem(&b, m.rootSelection == len(m.sections)+1, "Exit without saving", "")
	case configMenuSection:
		section := m.sections[m.section]
		fields := m.visibleSectionFields()
		fmt.Fprintf(&b, "%s settings\n\n", section.title)
		for i, field := range fields {
			value := configMenuFieldPreview(m.cfg, field.key)
			m.writeMenuItem(&b, i == m.sectionSelection, field.label, value)
		}
		fmt.Fprintln(&b)
		m.writeMenuItem(&b, m.sectionSelection == len(fields), "< Back", "")
	case configMenuSelect:
		fmt.Fprintf(&b, "Select %s\n\n", m.field.label)
		for i, option := range m.field.options {
			m.writeMenuItem(&b, i == m.optionSelection, option.label, "")
		}
	case configMenuMultiSelect:
		fmt.Fprintf(&b, "Select %s\n\n", m.field.label)
		for i, option := range m.field.options {
			checked := " "
			if i < len(m.selectedOptions) && m.selectedOptions[i] {
				checked = "x"
			}
			m.writeMenuItem(&b, i == m.optionSelection, fmt.Sprintf("[%s] %s", checked, option.label), "")
		}
	case configMenuInput:
		fmt.Fprintf(&b, "Edit %s\n\n", m.field.label)
		fmt.Fprintln(&b, m.input.View())
	case configMenuText:
		fmt.Fprintf(&b, "Edit %s\n", m.field.label)
		fmt.Fprintln(&b, "Use one value per line. Ctrl-S applies; Esc cancels.")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, m.text.View())
	case configMenuExitConfirm:
		fmt.Fprintln(&b, "Unsaved changes")
		fmt.Fprintln(&b, "Save your changes before exiting?")
		fmt.Fprintln(&b)
		for i, item := range []string{"Save changes", "Discard changes", "Return to configuration"} {
			m.writeMenuItem(&b, i == m.exitSelection, item, "")
		}
	}

	if m.err != "" {
		fmt.Fprintf(&b, "\nError: %s\n", m.err)
	}
	fmt.Fprintln(&b)
	switch m.mode {
	case configMenuRoot:
		fmt.Fprintln(&b, "Up/Down move  Enter open  S save  Q quit without saving")
	case configMenuSection:
		fmt.Fprintln(&b, "Up/Down move  Enter edit  Esc back  S save  Q quit")
	case configMenuSelect:
		fmt.Fprintln(&b, "Up/Down select  Enter apply  Esc cancel  S save  Q quit")
	case configMenuMultiSelect:
		fmt.Fprintln(&b, "Up/Down move  Space toggle  Enter apply  Esc cancel")
	case configMenuInput:
		fmt.Fprintln(&b, "Type to edit  Enter apply  Esc cancel  Ctrl-C quit")
	case configMenuText:
		fmt.Fprintln(&b, "Ctrl-S apply  Esc cancel  Ctrl-C quit")
	case configMenuExitConfirm:
		fmt.Fprintln(&b, "Up/Down choose  Enter confirm  Esc return to configuration")
	}
	return b.String()
}

func cloneConfig(cfg Config) Config {
	cfg.Toolchains = cloneConfigStrings(cfg.Toolchains)
	cfg.AgentCmd = cloneConfigStrings(cfg.AgentCmd)
	cfg.Mounts = cloneConfigStrings(cfg.Mounts)
	cfg.Env = cloneConfigStrings(cfg.Env)
	cfg.AllowedDomains = cloneConfigStrings(cfg.AllowedDomains)
	return cfg
}

func cloneConfigStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func (m configMenuModel) writeMenuItem(b *strings.Builder, selected bool, label, value string) {
	marker := "  "
	if selected {
		marker = "> "
	}
	available := m.width - 32
	if available < 12 {
		available = 12
	}
	labelWidth := 25
	if labelWidth > available/2 {
		labelWidth = available / 2
	}
	fmt.Fprintf(b, "%s%-*s %s\n", marker, labelWidth, clipConfigText(label, labelWidth), clipConfigText(value, available-labelWidth-1))
}

func configMenuFieldText(cfg Config, key string) string {
	switch key {
	case "snapshot_dir":
		return cfg.SnapshotDir
	case "runtime":
		return cfg.Runtime
	case "image":
		return cfg.Image
	case "agent_cmd":
		return strings.Join(cfg.AgentCmd, "\n")
	case "shm_size":
		return cfg.ShmSize
	case "max_parallel":
		return strconv.Itoa(cfg.MaxParallel)
	case "mounts":
		return strings.Join(cfg.Mounts, "\n")
	case "env":
		return strings.Join(cfg.Env, "\n")
	case "network":
		return cfg.Network
	case "allowed_domains":
		return strings.Join(cfg.AllowedDomains, "\n")
	case "proxy_image":
		return cfg.ProxyImage
	case "egress_network":
		return cfg.EgressNetwork
	case "github_token":
		return cfg.GitHubToken
	case "toolchains":
		return strings.Join(cfg.Toolchains, "\n")
	case "custom_containerfile":
		return cfg.CustomContainerfile
	default:
		return ""
	}
}

func setConfigMenuField(cfg *Config, key, value string) {
	switch key {
	case "snapshot_dir":
		cfg.SnapshotDir = strings.TrimSpace(value)
	case "runtime":
		cfg.Runtime = value
	case "image":
		cfg.Image = strings.TrimSpace(value)
	case "agent_cmd":
		cfg.AgentCmd = configTextLines(value)
	case "shm_size":
		cfg.ShmSize = strings.TrimSpace(value)
	case "max_parallel":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			n = -1 // rejected by validateConfiguredValues
		}
		cfg.MaxParallel = n
	case "mounts":
		cfg.Mounts = configTextLines(value)
	case "env":
		cfg.Env = configTextLines(value)
	case "network":
		cfg.Network = value
	case "allowed_domains":
		cfg.AllowedDomains = configTextLines(value)
	case "proxy_image":
		cfg.ProxyImage = strings.TrimSpace(value)
	case "egress_network":
		cfg.EgressNetwork = strings.TrimSpace(value)
	case "github_token":
		cfg.GitHubToken = strings.TrimSpace(value)
	case "toolchains":
		cfg.Toolchains = configTextLines(value)
	case "custom_containerfile":
		cfg.CustomContainerfile = strings.TrimSpace(value)
	}
}

func configMenuFieldPreview(cfg Config, key string) string {
	switch key {
	case "runtime":
		if cfg.Runtime == "podman" {
			return "Podman"
		}
		if cfg.Runtime == "docker" {
			return "Docker"
		}
	case "network":
		switch cfg.Network {
		case "open":
			return "Open"
		case "none":
			return "None"
		case "proxy":
			return "Proxy"
		}
	case "agent_cmd", "mounts", "env", "allowed_domains":
		lines := configTextLines(configMenuFieldText(cfg, key))
		if len(lines) == 0 {
			return "(none)"
		}
		return fmt.Sprintf("%d entries", len(lines))
	case "github_token":
		if cfg.GitHubToken == "" {
			return "Not set"
		}
		return "Set in project config"
	case "toolchains":
		if len(cfg.Toolchains) == 0 {
			return "None (Playwright base)"
		}
		labels := make([]string, 0, len(cfg.Toolchains))
		for _, option := range toolchainMenuOptions() {
			if slices.Contains(cfg.Toolchains, option.value) {
				labels = append(labels, option.label)
			}
		}
		return strings.Join(labels, ", ")
	}
	return configMenuFieldText(cfg, key)
}

func clipConfigText(value string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxWidth {
		return value
	}
	if maxWidth <= 3 {
		return string(runes[:maxWidth])
	}
	return string(runes[:maxWidth-3]) + "..."
}

func positiveWidth(width int) int {
	if width < 20 {
		return 20
	}
	return width
}
