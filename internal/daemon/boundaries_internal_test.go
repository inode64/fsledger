package daemon

import (
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mountinfo"
)

func TestBoundaryMatcherUsesLiteralPathsAndPreservesBase(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New("/root/cache/**")
	if err != nil {
		t.Fatal(err)
	}

	runner := new(worker)
	runner.matcher = matcher

	scoped := runner.boundaryMatcher(
		"/root",
		[]mountinfo.Mount{{Point: "/root/disk[1]", Type: "test", Device: "", ID: 0}},
	)
	if !scoped.Match("/root/disk[1]/file") || scoped.Match("/root/disk1/file") || !scoped.Match("/root/cache/file") {
		t.Fatal("nested mount boundary was interpreted as a glob or base exclusions were lost")
	}

	if matcher.Match("/root/disk[1]/file") {
		t.Fatal("scoping mutated the shared matcher")
	}
}
