package integrity_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

const hashSHA256 = "sha256"

//nolint:gocognit // One scanner must survive cancellations, algorithm changes and successive file versions.
func TestScannerReusesHashStateSafely(t *testing.T) {
	t.Parallel()

	scanner := integrity.NewScanner(1, 2)
	path := filepath.Join(t.TempDir(), "file")

	for _, algorithm := range []string{hashSHA256, hashSHA256, "blake3", "xxhash64", hashSHA256} {
		for _, content := range []string{"first observation", "", "replacement"} {
			err := os.WriteFile(path, []byte(content), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			cancelled, cancel := context.WithCancel(t.Context())
			cancel()

			_, err = scanner.Observe(cancelled, path, algorithm, true)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled observation: %v", err)
			}

			record, err := scanner.Observe(t.Context(), path, algorithm, true)
			if err != nil {
				t.Fatal(err)
			}

			fresh, err := integrity.NewScanner(1, 2).Observe(t.Context(), path, algorithm, true)
			if err != nil {
				t.Fatal(err)
			}

			if record.Hash != fresh.Hash || record.Algorithm != algorithm {
				t.Fatalf("stale hash state for %s and %q", algorithm, content)
			}
		}
	}
}

func BenchmarkObserve(b *testing.B) {
	path := filepath.Join(b.TempDir(), "file")

	err := os.WriteFile(path, make([]byte, 4096), 0o600)
	if err != nil {
		b.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 2)

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_, err = scanner.Observe(b.Context(), path, "sha256", true)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashes(b *testing.B) {
	for _, algorithm := range []string{"sha256", "blake3", "xxhash64"} {
		b.Run(algorithm, func(b *testing.B) {
			data := make([]byte, 1<<20)

			_, err := rand.Read(data)
			if err != nil {
				b.Fatal(err)
			}

			digest, err := integrity.Hasher(algorithm)
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				digest.Reset()

				_, err = digest.Write(data)
				if err != nil {
					b.Fatal(err)
				}

				_ = digest.Sum(nil)
			}
		})
	}
}
