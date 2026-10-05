package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// config is the on-disk state for sticker pack management (data/sticker/config.json).
type config struct {
	DefaultPack string `json:"default_pack"`
}

func defaultConfig() config { return config{} }

// timeNowUnixNano is swappable in tests.
var timeNowUnixNano = func() int64 {
	return time.Now().UnixNano()
}

func loadConfig(path string) config {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaultConfig()
	}
	return cfg
}

// saveConfig writes config atomically (temp file + rename, 0600).
func saveConfig(path string, cfg config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

var errStopped = errors.New("plugin is stopping")
