package config

import "testing"

func TestFilesystemRootRejectedBeforeStarting(t *testing.T) {
	t.Parallel()

	cfg := defaults()

	err := cfg.validateSource("/", nil, nil, cfg.InternalPaths())
	if err == nil {
		t.Fatal("unsupported mirror root accepted")
	}
}
