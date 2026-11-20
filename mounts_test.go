package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEffectiveMountsCombineTemplatesAndCustom(t *testing.T) {
	cfg := &Config{MountTemplates: []string{"maven"}, Mounts: []string{"/a:/b:ro"}}
	want := []string{"~/.m2/repository:/home/agent/.m2/repository", "/a:/b:ro"}
	if got := cfg.effectiveMounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("effectiveMounts = %#v, want %#v", got, want)
	}
	if len(cfg.Mounts) != 1 {
		t.Fatalf("effectiveMounts modified cfg.Mounts: %#v", cfg.Mounts)
	}
}

func TestValidateMountTemplates(t *testing.T) {
	if err := validateMountTemplates([]string{"gradle", "npm"}); err != nil {
		t.Fatal(err)
	}
	if validateMountTemplates([]string{"bogus"}) == nil {
		t.Fatal("unknown template accepted")
	}
	if validateMountTemplates([]string{"npm", "npm"}) == nil {
		t.Fatal("duplicate template accepted")
	}
}

func TestEnsureTemplateMountDirsCreatesHostDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &Config{MountTemplates: []string{"gradle"}}
	if err := cfg.ensureTemplateMountDirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".gradle/caches", ".gradle/wrapper"} {
		if !dirExists(filepath.Join(home, d)) {
			t.Fatalf("%s not created", d)
		}
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
