package audit

import (
	"testing"

	"github.com/elastic/go-libaudit/v2/aucoalesce"

	"github.com/inode64/fsledger/internal/event"
)

func TestRelativeDirfdPathsAreNotGuessedFromCWD(t *testing.T) {
	t.Parallel()

	value := new(aucoalesce.Event)
	value.Process.CWD = "/unrelated"
	value.Data = map[string]string{"syscall": "unlinkat", "a0": "3"}

	value.Paths = []map[string]string{{"name": "gone", "nametype": "DELETE", "inode": "123", "dev": "00:01"}}
	if got := recordedPaths(value, event.Actor{}, event.Remove); len(got) != 0 {
		t.Fatal("invented CWD deletion", got)
	}

	value.Data["a0"] = "ffffff9c"
	if got := recordedPaths(value, event.Actor{}, event.Remove); len(got) != 1 || got[0].path != "/unrelated/gone" {
		t.Fatal(got)
	}

	value.Data["a0"] = "3"

	value.Paths[0]["name"] = "/absolute/gone"
	if got := recordedPaths(value, event.Actor{}, event.Remove); len(got) != 1 || got[0].path != "/absolute/gone" {
		t.Fatal(got)
	}
}
