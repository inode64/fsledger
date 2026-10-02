package daemon

import (
	"time"

	"github.com/inode64/fsledger/internal/integrity"
)

// Path instability postpones an observation; it is not a loss of event attribution.
func (w *worker) deferUnstable(err error) error {
	paths, failure := integrity.SplitUnstable(err)
	for _, path := range paths {
		if w.unstable == nil {
			w.unstable = make(map[string]struct{})
		}

		if _, exists := w.unstable[path]; !exists {
			w.logger.Warn("source remains unstable; observation deferred", "path", path)
		}

		w.unstable[path] = struct{}{}
		if w.scanningUnstable != nil {
			w.scanningUnstable[path] = struct{}{}
		}
	}

	if len(paths) > 0 && w.unstableRetry.IsZero() {
		w.unstableRetry = time.Now().Add(maximumReconcileBackoff)
	}

	return failure
}

func (w *worker) finishUnstableScan() {
	w.unstable = w.scanningUnstable

	w.unstableRetry = time.Time{}
	if len(w.unstable) > 0 {
		w.unstableRetry = time.Now().Add(maximumReconcileBackoff)
	}
}
