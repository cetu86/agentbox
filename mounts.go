package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
)

// mountTemplate is a named set of cache mounts that users can select
// instead of typing the mount specs by hand. Host directories are created
// on demand so the runtime does not create them as root.
type mountTemplate struct {
	value  string
	label  string
	mounts []string
}

const containerHome = "/home/agent"

var mountTemplates = []mountTemplate{
	{value: "gradle", label: "Gradle caches and wrapper (~/.gradle)", mounts: []string{
		"~/.gradle/caches:" + containerHome + "/.gradle/caches",
		"~/.gradle/wrapper:" + containerHome + "/.gradle/wrapper",
	}},
	{value: "maven", label: "Maven repository (~/.m2/repository)", mounts: []string{
		"~/.m2/repository:" + containerHome + "/.m2/repository",
	}},
	{value: "npm", label: "npm cache (~/.npm)", mounts: []string{
		"~/.npm:" + containerHome + "/.npm",
	}},
	{value: "pip", label: "pip cache (~/.cache/pip)", mounts: []string{
		"~/.cache/pip:" + containerHome + "/.cache/pip",
	}},
}

func mountTemplateMenuOptions() []configMenuOption {
	options := make([]configMenuOption, 0, len(mountTemplates))
	for _, t := range mountTemplates {
		options = append(options, configMenuOption{label: t.label, value: t.value})
	}
	return options
}

func mountTemplateHuhOptions() []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(mountTemplates))
	for _, t := range mountTemplates {
		options = append(options, huh.NewOption(t.label, t.value))
	}
	return options
}

func validateMountTemplates(values []string) error {
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if seen[v] {
			return fmt.Errorf("mount template %q is selected more than once", v)
		}
		seen[v] = true
		found := false
		for _, t := range mountTemplates {
			if t.value == v {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unsupported mount template %q", v)
		}
	}
	return nil
}

// templateMounts returns the mounts of the selected templates.
func templateMounts(selected []string) []string {
	var out []string
	for _, t := range mountTemplates {
		if containsString(selected, t.value) {
			out = append(out, t.mounts...)
		}
	}
	return out
}

// effectiveMounts returns template mounts followed by the user's own.
func (c *Config) effectiveMounts() []string {
	return append(templateMounts(c.MountTemplates), c.Mounts...)
}

// ensureTemplateMountDirs creates the host side of template mounts.
func (c *Config) ensureTemplateMountDirs() error {
	for _, m := range templateMounts(c.MountTemplates) {
		host, _, _ := strings.Cut(m, ":")
		host = expandHome(host)
		if err := os.MkdirAll(host, 0o755); err != nil {
			return fmt.Errorf("create mount directory %s: %w", host, err)
		}
	}
	return nil
}
