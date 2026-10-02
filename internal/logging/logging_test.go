package logging_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/logging"
)

const infoLevel = "info"

func TestLevels(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		level config.LogLevel
		debug bool
		count int
	}{
		{"debug", false, 4}, {infoLevel, false, 3}, {"warning", false, 2}, {"error", false, 1}, {"error", true, 4},
	} {
		t.Run(string(testCase.level)+strconv.Itoa(testCase.count), func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer

			logger, err := logging.Open(config.Logging{Level: testCase.level, File: ""}, &output, testCase.debug)
			if err != nil {
				t.Fatal(err)
			}

			logger.Debug("debug fixture")
			logger.Info("info fixture")
			logger.Warn("warning fixture")
			logger.Error("error fixture")

			closeErr := logger.Close()
			if closeErr != nil {
				t.Fatal(closeErr)
			}

			if strings.Count(output.String(), "\n") != testCase.count ||
				!strings.Contains(output.String(), "error fixture") {
				t.Fatal(output.String())
			}
		})
	}
}

func TestFileAppend(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "logs", "daemon.log")

	var stderr bytes.Buffer
	for range 2 {
		logger, err := logging.Open(config.Logging{Level: infoLevel, File: path}, &stderr, false)
		if err != nil {
			t.Fatal(err)
		}

		logger.Info("persistent fixture")
		logger.Debug("filtered fixture")

		closeErr := logger.Close()
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	//nolint:gosec // Read only the log created in this test's private temporary directory.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Count(string(data), "persistent fixture") != 2 || strings.Contains(string(data), "filtered") ||
		stderr.Len() != 0 {
		t.Fatalf("file=%s stderr=%s", data, stderr.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions=%v", info.Mode())
	}
}

func TestRejectDestination(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	link := filepath.Join(base, "link")

	symlinkErr := os.Symlink(filepath.Join(base, "target"), link)
	if symlinkErr != nil {
		t.Fatal(symlinkErr)
	}

	for _, path := range []string{base, link} {
		var stderr bytes.Buffer

		logger, err := logging.Open(config.Logging{Level: infoLevel, File: path}, &stderr, false)
		if err == nil {
			closeErr := logger.Close()
			if closeErr != nil {
				t.Fatal(closeErr)
			}

			t.Fatal("accepted non-regular destination")
		}
	}
}
