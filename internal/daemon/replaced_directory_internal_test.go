package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/mirror"
)

// rm -r dir && ln -s releases dir reaches the worker as one event group holding the children of dir and
// dir itself. They are committed together, under the actor that made the change.
func TestDirectoryReplacedBySymlinkKeepsItsActor(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	directory := filepath.Join(runner.status.Paths[0], "dir")

	err := os.Mkdir(directory, 0o700)
	if err == nil {
		err = os.Mkdir(filepath.Join(runner.status.Paths[0], "releases"), 0o700)
	}

	if err != nil {
		t.Fatal(err)
	}

	child := writeSource(t, runner, "dir/file", "one")
	writeSource(t, runner, "releases/file", "two")

	err = runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	err = os.RemoveAll(directory)
	if err == nil {
		err = os.Symlink("releases", directory)
	}

	if err != nil {
		t.Fatal(err)
	}

	editor := event.Actor{PID: 1, StartTime: 1, UserKnown: true, Known: true}
	for _, path := range []string{child, directory} {
		runner.accept(event.Raw{Path: path, Backend: event.Fanotify, Actor: editor})
	}

	runner.flush(t.Context(), time.Now().Add(time.Second))

	if runner.dirty || runner.contaminated || len(runner.status.Warnings) != 0 {
		t.Fatal("the replacement was not committed with its group", runner.status.Warnings)
	}

	subject := gitOutput(t, runner, "log", "-1", "--format=%s")
	if strings.HasPrefix(subject, "unknown:") {
		t.Fatalf("the replacement lost its actor: %s", subject)
	}

	relative, err := mirror.Relative(directory)
	if err != nil {
		t.Fatal(err)
	}

	tree := gitOutput(t, runner, "ls-tree", "-r", "--format=%(objectmode) %(path)", "HEAD", "--", relative)
	if tree != "120000 "+relative+"\n" {
		t.Fatalf("unexpected tree below the replaced directory:\n%s", tree)
	}
}
