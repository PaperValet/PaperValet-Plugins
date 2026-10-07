package main

import (
	"encoding/json"
	"os"
	"time"
)

// legacyConfig is the pre-settings storage for the default pack
// (data/sticker/config.json), read once by migrateLegacyConfig.
type legacyConfig struct {
	DefaultPack string `json:"default_pack"`
}

// timeNowUnixNano is swappable in tests.
var timeNowUnixNano = func() int64 {
	return time.Now().UnixNano()
}

// loadConfig reads a legacy config file; missing or corrupt files give an
// empty config, which migration treats as "nothing to carry over".
func loadConfig(path string) legacyConfig {
	var cfg legacyConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg) // corrupt → empty → dropped
	return cfg
}
