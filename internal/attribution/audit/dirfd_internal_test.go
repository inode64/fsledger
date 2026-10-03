package audit

import (
	"testing"

	"github.com/elastic/go-libaudit/v2/aucoalesce"

	"github.com/inode64/fsledger/internal/event"
)

const (
	pathName      = "name"
	pathNameType  = "nametype"
	pathInode     = "inode"
	pathDevice    = "dev"
	fixtureDevice = "00:01"
)

func TestRelativeDirfdPathsAreNotGuessedFromCWD(t *testing.T) {
	t.Parallel()

	value := new(aucoalesce.Event)
	value.Process.CWD = "/unrelated"
	value.Data = map[string]string{"syscall": "unlinkat", "a0": "3"}

	value.Paths = []map[string]string{
		{pathName: "gone", pathNameType: "DELETE", pathInode: "123", pathDevice: fixtureDevice},
	}
	if got := recordedPaths(value, event.Actor{}, event.Remove); len(got) != 0 {
		t.Fatal("invented CWD deletion", got)
	}

	value.Data["a0"] = "ffffff9c"
	if got := recordedPaths(value, event.Actor{}, event.Remove); len(got) != 1 || got[0].path != "/unrelated/gone" {
		t.Fatal(got)
	}

	value.Data["a0"] = "3"

	value.Paths[0][pathName] = "/absolute/gone"
	if got := recordedPaths(value, event.Actor{}, event.Remove); len(got) != 1 || got[0].path != "/absolute/gone" {
		t.Fatal(got)
	}
}

func TestHardLinkSourceCannotMatchContentWrite(t *testing.T) {
	t.Parallel()

	for _, syscall := range []string{"link", "linkat"} {
		value := new(aucoalesce.Event)
		value.Data = map[string]string{"syscall": syscall}
		value.Paths = []map[string]string{
			{pathName: "/source", pathNameType: "NORMAL", pathInode: "123", pathDevice: fixtureDevice},
			{pathName: "/linked", pathNameType: "CREATE", pathInode: "123", pathDevice: fixtureDevice},
		}

		observed := recordedPaths(value, event.Actor{}, auditOperation(value.Data))
		if len(observed) != 2 || observed[0].operation != event.Attrib || observed[1].operation != event.Create {
			t.Fatal("hard link paths have incorrect operations", syscall, observed)
		}

		if matches(observed[0], event.Raw{Path: "/source", Time: value.Timestamp, Operation: event.Write}) {
			t.Fatal("link source matched an unrelated content write", syscall)
		}
	}
}
