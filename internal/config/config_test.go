package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAndLoad(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nested", "config.toml")
	want := Config{Version: 1, AccountID: "account-id", VaultID: "vault-id"}
	if err := Save(file, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(file)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("configuration permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadIgnoresLegacyThemePreference(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte("version = 1\naccount_id = 'account-id'\nvault_id = 'vault-id'\ntheme = 'dark'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (Config{Version: 1, AccountID: "account-id", VaultID: "vault-id"}) {
		t.Fatalf("Load() = %#v", cfg)
	}
}

func TestSaveDoesNotPersistTheme(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(file, Config{Version: 1, AccountID: "account-id", VaultID: "vault-id"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "theme") {
		t.Fatalf("config persisted removed theme preference: %q", contents)
	}
}

func TestLoadMissingConfigSuggestsInit(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err == nil || err.Error() != "configuration not found: run `opfm init`" {
		t.Fatalf("Load() error = %v", err)
	}
}
