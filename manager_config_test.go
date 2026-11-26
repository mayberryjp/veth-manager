package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerConfigOptInAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, text               string
		rename, cleanup, invalid bool
	}{
		{"disabled", "renaming: {}\norphan_cleanup: {}\n", false, false, false},
		{"rename only", "renaming:\n  enabled: true\n", true, false, false},
		{"cleanup only", "orphan_cleanup:\n  enabled: true\n  interval: 2s\n  min_age: 3s\n", false, true, false},
		{"both", "renaming:\n  enabled: true\norphan_cleanup:\n  enabled: true\n", true, true, false},
		{"unknown section", "cleanup: {}\n", false, false, true},
		{"unknown field", "orphan_cleanup:\n  typo: true\n", false, false, true},
		{"duplicate section", "renaming: {}\nrenaming: {}\n", false, false, true},
		{"duplicate field", "renaming:\n  enabled: true\n  enabled: false\n", false, false, true},
		{"null section", "renaming: null\n", false, false, true},
		{"null boolean", "renaming:\n  enabled: null\n", false, false, true},
		{"quoted boolean", "renaming:\n  enabled: 'true'\n", false, false, true},
		{"numeric duration", "orphan_cleanup:\n  interval: 30\n", false, false, true},
		{"bad duration", "orphan_cleanup:\n  interval: never\n", false, false, true},
		{"too short", "orphan_cleanup:\n  interval: 500ms\n", false, false, true},
		{"negative age", "orphan_cleanup:\n  min_age: -1s\n", false, false, true},
		{"age below interval", "orphan_cleanup:\n  interval: 5m\n  min_age: 1m\n", false, false, true},
		{"multiple documents", "renaming: {}\n---\nrenaming: {}\n", false, false, true},
		{"JSON", `{"renaming":{"enabled":true}}`, false, false, true},
		{"alias", "renaming: &section {}\norphan_cleanup: *section\n", false, false, true},
		{"empty", "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manager.yaml")
			if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := loadManagerConfig(path, true)
			if (err != nil) != tc.invalid {
				t.Fatalf("config=%+v err=%v", got, err)
			}
			if !tc.invalid && (got.Renaming.Enabled != tc.rename || got.OrphanCleanup.Enabled != tc.cleanup) {
				t.Fatalf("unexpected opt-in state: %+v", got)
			}
			if tc.name == "cleanup only" && (got.OrphanCleanup.interval != 2*time.Second || got.OrphanCleanup.minAge != 3*time.Second) {
				t.Fatalf("configured cleanup timing ignored: %+v", got)
			}
		})
	}
}

func TestManagerMissingConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.yaml")
	c, err := loadManagerConfig(path, false)
	if err != nil || c.Renaming.Enabled || c.OrphanCleanup.Enabled {
		t.Fatalf("missing default config enabled mutations: %+v %v", c, err)
	}
	if _, err := loadManagerConfig(path, true); err == nil {
		t.Fatal("explicit missing config silently ignored")
	}
	if _, err := loadManagerConfig(filepath.Join(t.TempDir(), "manager.json"), false); err == nil {
		t.Fatal("non-YAML configuration accepted")
	}
}
