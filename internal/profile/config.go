// Package profile persists client settings and history, never dictionary assets.
package profile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Dictionary string `json:"dictionary"`
	Audio      string `json:"audio"`
	Theme      string `json:"theme"`
	Language   string `json:"language"`
	Glyphs     string `json:"glyphs"`
}

func DefaultPath() (string, error) {
	directory, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, ".config", "stupidict", "config.json"), nil
}

func Defaults() Config {
	return Config{Dictionary: "dictionary.db", Audio: "audio-pack.db", Theme: "system", Language: "system", Glyphs: "plain"}
}

// Normalize makes asset paths independent of the launch directory. Relative
// paths in JSON are anchored to the configuration file; ~/ is also supported.
func (config Config) Normalize(path string) (Config, error) {
	if config.Theme != "system" && config.Theme != "dark" && config.Theme != "light" {
		return config, errors.New("theme must be system, dark or light")
	}
	if config.Language != "system" && config.Language != "zh" && config.Language != "en" {
		return config, errors.New("language must be system, zh or en")
	}
	if config.Glyphs != "plain" && config.Glyphs != "nerd" && config.Glyphs != "emoji" {
		return config, errors.New("glyphs must be plain, nerd or emoji")
	}
	for _, field := range []*string{&config.Dictionary, &config.Audio} {
		value := strings.TrimSpace(*field)
		if value == "" {
			*field = ""
			continue
		}
		if value == "~" || strings.HasPrefix(value, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return config, err
			}
			value = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(value, "~"), "/"))
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(filepath.Dir(path), value)
		}
		absolute, err := filepath.Abs(value)
		if err != nil {
			return config, err
		}
		*field = absolute
	}
	if config.Dictionary == "" {
		return config, errors.New("dictionary path is required")
	}
	configPath, err := filepath.Abs(path)
	if err != nil {
		return config, err
	}
	historyPath := filepath.Join(filepath.Dir(configPath), "history.db")
	for _, asset := range []string{config.Dictionary, config.Audio} {
		if asset == configPath || asset == historyPath {
			return config, errors.New("asset paths must not overwrite configuration or history")
		}
	}
	if config.Dictionary == config.Audio {
		return config, errors.New("dictionary and audio paths must differ")
	}
	return config, nil
}

func Load(path string) (Config, error) {
	config := Defaults()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config.Normalize(path)
	}
	if err != nil {
		return config, err
	}
	// Invalid files are reported, not silently replaced with defaults.
	if strings.TrimSpace(string(data)) == "null" {
		return config, errors.New("configuration must be a JSON object")
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, err
	}
	return config.Normalize(path)
}

func Save(path string, config Config) error {
	config, err := config.Normalize(path)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	// The writer must be closed before replacement on Windows.
	return os.Rename(file.Name(), path)
}
