package catalog

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble/v2"
)

type engineLogger struct{ repository string }

func (logger engineLogger) Infof(format string, arguments ...any) {
	slog.Debug("catalog engine", "repository", logger.repository, "detail", fmt.Sprintf(format, arguments...))
}

func (logger engineLogger) Errorf(format string, arguments ...any) {
	slog.Error("catalog engine", "repository", logger.repository, "detail", fmt.Sprintf(format, arguments...))
}

func (logger engineLogger) Fatalf(format string, arguments ...any) {
	// Preserve Pebble's fail-stop contract; returning would let it continue after failed durability.
	logger.Errorf(format, arguments...)
	pebble.DefaultLogger.Fatalf(format, arguments...)
}
