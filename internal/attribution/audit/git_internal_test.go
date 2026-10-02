package audit

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/grouping"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
)

//nolint:funlen,gocognit,gocyclo,cyclop // Keep the complete replay-to-Git integration lifecycle ordered.
func TestAuditReplayProducesSeparateGitCommits(t *testing.T) {
	t.Parallel()
	paths := []string{auditFixture(t), auditFixture(t)}
	repo := &gitrepo.Repository{Path: t.TempDir(), Host: "audit-test", Timeout: 0}

	_, err := repo.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := mirror.Open(repo.Path, paths, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(syncer)

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_, err = repo.Commit(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	provider := processor(t)
	manager := grouping.New(config.Grouping{
		ByProcess: true, ByUser: true, PreferLoginUID: true,
	}, time.Second, 3*time.Second)
	observedAt := time.Now().Add(-2 * matchWindow)

	for index, path := range paths {
		err = os.WriteFile(path, []byte(fmt.Sprintf("actor %d", index)), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		manager.Add(event.Raw{Path: path, Time: observedAt, Backend: event.Inotify, Operation: event.Write})
		pushLines(t, provider, fixture(t, observedAt, 40+index, 1001+index, path))
	}

	manager.Enrich(func(raw event.Raw) event.Actor { return provider.Resolve(t.Context(), raw, "fsledger") })

	groups := manager.Due(time.Now(), true)
	if len(groups) != 2 {
		t.Fatalf("Audit failed to separate actors: %+v", groups)
	}

	for _, group := range groups {
		if !group.Actor.Known || len(group.Paths) != 1 {
			t.Fatalf("wrong Audit group: %+v", group)
		}

		for path := range group.Paths {
			err = syncer.ApplyPaths(t.Context(), []string{path})
			if err != nil {
				t.Fatal(err)
			}
		}

		_, err = repo.Commit(t.Context(), fmt.Sprintf("audit uid=%d %s", group.Actor.UID, group.Actor.Evidence))
		if err != nil {
			t.Fatal(err)
		}
	}
	//nolint:gosec // Literal Git command reads only the private temporary repository.
	output, err := exec.CommandContext(t.Context(), "git", "-C", repo.Path, "log", "--format=%s").Output()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(output), "uid=1001") || !strings.Contains(string(output), "uid=1002") ||
		len(strings.Split(strings.TrimSpace(string(output)), "\n")) != 3 {
		t.Fatalf("wrong Git history: %s", output)
	}
}
