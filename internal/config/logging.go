package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/inode64/fsledger/internal/fault"
)

// LogLevel is the severity name accepted by the YAML configuration.
type LogLevel string

// Supported log levels preserve the public YAML spelling, including warning.
const (
	LogLevelDebug   LogLevel = "debug"
	LogLevelInfo    LogLevel = "info"
	LogLevelWarning LogLevel = "warning"
	LogLevelError   LogLevel = "error"
)

// SlogLevel validates and converts a configured level in one place.
func (level LogLevel) SlogLevel() (slog.Level, error) {
	switch level {
	case LogLevelDebug:
		return slog.LevelDebug, nil
	case LogLevelInfo:
		return slog.LevelInfo, nil
	case LogLevelWarning:
		return slog.LevelWarn, nil
	case LogLevelError:
		return slog.LevelError, nil
	default:
		return 0, fault.New("logging.level must be debug, info, warning or error")
	}
}

func (c *Config) validateLogging() error {
	_, err := c.Logging.Level.SlogLevel()
	if err != nil {
		return err
	}

	if c.Logging.File == "" {
		return nil
	}

	absoluteErr := CleanAbsolute(c.Logging.File)
	if absoluteErr != nil {
		return absoluteErr
	}

	for _, reserved := range []string{c.Storage.Path, c.Runtime} {
		if Overlaps(reserved, c.Logging.File) {
			return fault.New("log file overlaps storage/runtime")
		}
	}

	return nil
}

// CheckLogPath inspects existing parents and the destination without creating files.
func CheckLogPath(path string) error {
	parentErr := checkStorage(filepath.Dir(path))
	if parentErr != nil {
		return parentErr
	}

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fault.Wrap("inspect log file", err)
	}

	if !info.Mode().IsRegular() {
		return fault.New("log file must be a regular file, not a symlink or special file")
	}

	return nil
}
