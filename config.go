package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AppConfig holds all persistent user settings.
type AppConfig struct {
	Name         string   `json:"name"`
	WebPort      int      `json:"webPort"`
	LogLevel     string   `json:"logLevel"`
	WindowWidth  int      `json:"windowWidth"`
	WindowHeight int      `json:"windowHeight"`
	BlockedUsers []string `json:"blockedUsers"`
	SaveHistory  *bool    `json:"saveHistory"` // nil = true (default on)
}

// IsSaveHistory returns whether chat history should be saved (default true).
func (c *AppConfig) IsSaveHistory() bool {
	return c.SaveHistory == nil || *c.SaveHistory
}

var (
	appDataDir     string
	appDataDirOnce sync.Once
)

// DefaultConfig returns config with default values.
func DefaultConfig() *AppConfig {
	return &AppConfig{
		WebPort:      8080,
		LogLevel:     "error",
		WindowWidth:  1000,
		WindowHeight: 700,
	}
}

// AppDataDir returns the path to ~/.lanshare/, creating it if needed.
func AppDataDir() string {
	appDataDirOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fallback to exe directory
			if exe, err2 := os.Executable(); err2 == nil {
				appDataDir = filepath.Dir(exe)
			} else {
				appDataDir = "."
			}
			return
		}
		appDataDir = filepath.Join(home, ".lanshare")
		os.MkdirAll(appDataDir, 0755)
	})
	return appDataDir
}

// DataPath returns the full path for a file inside the data directory.
func DataPath(elem ...string) string {
	parts := append([]string{AppDataDir()}, elem...)
	return filepath.Join(parts...)
}

// configPath returns the config file path.
func configPath() string {
	return DataPath("config.json")
}

// LoadConfig reads config from ~/.lanshare/config.json.
// Returns default config if file doesn't exist.
func LoadConfig() *AppConfig {
	cfg := DefaultConfig()

	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		fmt.Printf("配置文件解析失败，使用默认配置: %v\n", err)
		return DefaultConfig()
	}

	// Ensure window size has valid defaults
	if cfg.WindowWidth <= 0 {
		cfg.WindowWidth = 1000
	}
	if cfg.WindowHeight <= 0 {
		cfg.WindowHeight = 700
	}

	return cfg
}

// SaveConfig writes the config to ~/.lanshare/config.json.
func SaveConfig(cfg *AppConfig) error {
	os.MkdirAll(AppDataDir(), 0755)

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath(), data, 0644)
}
