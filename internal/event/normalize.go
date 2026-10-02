package event

import (
	"time"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/fault"
)

// Normalize rejects malformed detector paths before grouping or filesystem access.
func Normalize(raw Raw) (Raw, error) {
	for index, path := range []string{raw.Path, raw.OldPath} {
		if path == "" && index == 1 {
			continue
		}

		if !pathutil.ValidAbsolute(path) {
			return Raw{}, fault.New("detector returned a noncanonical absolute path")
		}
	}

	if raw.Time.IsZero() {
		raw.Time = time.Now()
	}

	if !raw.Actor.Known {
		raw.Actor = Actor{}
	}

	return raw, nil
}
