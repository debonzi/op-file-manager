// Package config loads the deliberately small, non-secret opfm configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

const fileName = "config.toml"

// Config contains non-secret identifiers. Session tokens and Document contents
// must never be persisted here.
type Config struct {
	Version   int    `toml:"version"`
	AccountID string `toml:"account_id"`
	VaultID   string `toml:"vault_id"`
}

func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "opfm", fileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "opfm", fileName), nil
}

func Load(file string) (Config, error) {
	contents, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, fmt.Errorf("configuration not found: run `opfm init`")
		}
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(contents, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported configuration version %d", c.Version)
	}
	if c.AccountID == "" || c.VaultID == "" {
		return errors.New("configuration requires account_id and vault_id")
	}
	return nil
}

func Save(file string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	contents, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}

	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create temporary configuration: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("protect temporary configuration: %w", err)
	}
	if _, err := temp.Write(contents); err != nil {
		temp.Close()
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err := os.Rename(tempName, file); err != nil {
		return fmt.Errorf("replace configuration: %w", err)
	}
	return os.Chmod(file, 0o600)
}
