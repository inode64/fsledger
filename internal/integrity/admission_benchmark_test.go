package integrity_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

func BenchmarkConcurrentScans(b *testing.B) {
	roots := scanBenchmarkRoots(b)

	matcher, err := exclude.New()
	if err != nil {
		b.Fatal(err)
	}

	for _, limit := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("limit-%d", limit), func(b *testing.B) {
			scanner := integrity.NewScanner(2, limit)

			b.ReportAllocs()

			for b.Loop() {
				benchmarkScanBatch(b, scanner, roots, matcher)
			}
		})
	}
}

func scanBenchmarkRoots(b *testing.B) []string {
	b.Helper()

	const repositories = 8

	roots := make([]string, repositories)
	for index := range roots {
		roots[index] = b.TempDir()
		for file := range 100 {
			err := os.WriteFile(filepath.Join(roots[index], fmt.Sprintf("file-%d", file)), []byte("payload"), 0o600)
			if err != nil {
				b.Fatal(err)
			}
		}
	}

	return roots
}

func benchmarkScanBatch(b *testing.B, scanner *integrity.Scanner, roots []string, matcher *exclude.Matcher) {
	b.Helper()

	var wait sync.WaitGroup

	failures := make(chan error, len(roots))
	for _, root := range roots {
		wait.Go(func() {
			failures <- scanner.ScanPaths(b.Context(), []string{root}, nil, matcher, "sha256", false,
				func(integrity.Record) error { return nil })
		})
	}

	wait.Wait()
	close(failures)

	for failure := range failures {
		if failure != nil {
			b.Fatal(failure)
		}
	}
}
