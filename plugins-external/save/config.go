package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// UserConfig is the per-user save configuration.
type UserConfig struct {
	Target     string `json:"target"`      // me | local | @user | user | chat id
	ShowSource bool   `json:"show_source"` // reply a source card after forwarding
}

// SaveDB is persisted in data/save/config.json.
type SaveDB struct {
	Users map[string]UserConfig `json:"users"`
}

func defaultConfig() UserConfig { return UserConfig{Target: "me"} }

func readDB(path string) (*SaveDB, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var db SaveDB
	if err := json.Unmarshal(b, &db); err != nil {
		return nil, err
	}
	if db.Users == nil {
		db.Users = map[string]UserConfig{}
	}
	return &db, nil
}

// loadDB reads the config, falling back to the legacy data/save_db.json.
func loadDB(path, legacy string) *SaveDB {
	if db, err := readDB(path); err == nil {
		return db
	}
	if legacy != "" {
		if db, err := readDB(legacy); err == nil {
			return db
		}
	}
	return &SaveDB{Users: map[string]UserConfig{}}
}

// writeDB writes atomically (temp file + rename).
func writeDB(path string, db *SaveDB) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(name)
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

var reTargetUser = regexp.MustCompile(`^@?[A-Za-z][A-Za-z0-9_]{3,31}$`)

// normalizeTarget validates a target and returns its canonical form.
func normalizeTarget(t string) (string, error) {
	t = strings.TrimSpace(t)
	low := strings.ToLower(t)
	switch low {
	case "", "me", "self", "saved":
		return "me", nil
	case "local":
		return "local", nil
	}
	if _, err := strconv.ParseInt(t, 10, 64); err == nil {
		return t, nil
	}
	for _, p := range []string{"https://t.me/", "http://t.me/", "t.me/"} {
		if strings.HasPrefix(low, p) {
			t = t[len(p):]
			break
		}
	}
	if reTargetUser.MatchString(t) {
		return "@" + strings.TrimPrefix(t, "@"), nil
	}
	return "", errors.New("invalid target")
}

func isLocal(t string) bool { return strings.EqualFold(strings.TrimSpace(t), "local") }
