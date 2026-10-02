package logging

import (
	"log"
	"log/slog"
)

// UseDefault redirects process-wide cleanup logs and returns their restoration.
// Call during CLI startup and restore after its workers stop, before closing the file.
func (logger *Logger) UseDefault() func() {
	previous := slog.Default()
	previousOutput, previousFlags := log.Writer(), log.Flags()

	slog.SetDefault(logger.Logger)

	return func() {
		slog.SetDefault(previous)
		// SetDefault also redirects the standard logger, including slog's original handler.
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	}
}
