package git

import (
	"bytes"
	"context"
	"strings"
	"unicode/utf8"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/redact"
)

const (
	gitModeAbsent     = "000000"
	gitModeRegular    = "100644"
	gitModeExecutable = "100755"
)

// StagedText reads immutable blob IDs from a captured raw diff, never source files.
// Nonregular, binary and oversized entries have no outbound text.
func (r *Repository) StagedText(ctx context.Context, signature string) (string, string, bool, error) {
	fields := strings.Fields(signature)
	if len(fields) != stagedFieldCount {
		return "", "", false, fault.New("invalid staged version")
	}

	for _, mode := range []string{strings.TrimPrefix(fields[0], ":"), fields[1]} {
		if mode != gitModeAbsent && mode != gitModeRegular && mode != gitModeExecutable {
			return "", "", false, nil
		}
	}

	before, eligible, err := r.smallBlob(ctx, fields[2])
	if err != nil || !eligible {
		return "", "", false, err
	}

	after, eligible, err := r.smallBlob(ctx, fields[3])

	return before, after, eligible, err
}

func (r *Repository) smallBlob(ctx context.Context, identifier string) (string, bool, error) {
	if strings.Trim(identifier, "0") == "" {
		return "", true, nil
	}

	content, eligible, err := r.boundedBlob(ctx, identifier, redact.MaxTextBytes)
	if err != nil || !eligible {
		return "", false, err
	}

	if !utf8.Valid(content) || bytes.ContainsRune(content, 0) {
		return "", false, nil
	}

	return string(content), true, nil
}
