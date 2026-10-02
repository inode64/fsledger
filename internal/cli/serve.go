package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
	"github.com/inode64/fsledger/internal/logging"
	"github.com/inode64/fsledger/internal/resource"
)

func serve(stderr io.Writer, cfg *config.Config, debug bool) error {
	logger, err := logging.Open(cfg.Logging, stderr, debug)
	if err != nil {
		return err
	}
	defer resource.Close(logger)

	restoreLogging := logger.UseDefault()
	defer restoreLogging()

	ctx, stop := signalContext(logger.Logger)
	defer stop()

	err = daemon.Run(ctx, cfg, logger.Logger)
	if err != nil {
		logger.Error("daemon failed", "error", err)
	}

	return err
}

// signalContext joins its SIGHUP listener before the caller closes logging.
func signalContext(logger *slog.Logger) (context.Context, func()) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	done := make(chan struct{})
	go func() {
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				cancel() // Restore default SIGINT/SIGTERM handling during the graceful drain.

				return
			case <-hup:
				logger.Info("SIGHUP received; restart required for configuration changes")
			}
		}
	}()

	return ctx, func() {
		signal.Stop(hup)
		cancel()
		<-done
	}
}
