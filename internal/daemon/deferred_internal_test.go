package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/filter"
)

const noisePattern = "**/noise"

func deferFixture(t *testing.T, rules []filter.Rule) (*worker, string, string) {
	t.Helper()
	runner := makeWorker(t)
	noise := writeSource(t, runner, "noise", "initial")
	normal := writeSource(t, runner, "normal", "initial")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	runner.cfg.Commit.Defer = rules

	err = runner.loadDeferral(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	return runner, noise, normal
}

func commitEvent(t *testing.T, runner *worker, path string, actor event.Actor) {
	t.Helper()

	now := time.Now()
	group := event.Group{Actor: actor, Paths: map[string]event.Raw{path: {Path: path, Actor: actor, Time: now}}}

	err := runner.commit(t.Context(), group)
	if err != nil {
		t.Fatal(err)
	}
}

func commitCount(t *testing.T, runner *worker) string {
	t.Helper()

	return strings.TrimSpace(gitOutput(t, runner, "rev-list", "--count", "HEAD"))
}

func TestDeferredPathsAccumulateAcrossReconciliationAndReload(t *testing.T) {
	t.Parallel()

	runner, noise, normal := deferFixture(
		t,
		[]filter.Rule{{Paths: []string{noisePattern}, Users: nil, CommandRegex: nil}},
	)
	for _, value := range []string{"one", "two", "three"} {
		writeSource(t, runner, "noise", value)
		commitEvent(t, runner, noise, event.Actor{})
	}

	if commitCount(t, runner) != "1" || len(runner.deferred) != 1 {
		t.Fatal("noise generated commits")
	}

	err := runner.reconcile(t.Context(), "periodic")
	if err != nil {
		t.Fatal(err)
	}

	runner.deferred = nil
	runner.status.LastCommit = ""

	err = runner.loadDeferral(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "startup")
	if err != nil {
		t.Fatal(err)
	}

	if commitCount(t, runner) != "1" || runner.status.LastCommit == "" {
		t.Fatal("restart flushed pending or lost publication head")
	}

	writeSource(t, runner, "normal", "trigger")
	commitEvent(t, runner, normal, event.Actor{Known: true, UserKnown: true, UID: 1001})

	if commitCount(t, runner) != "2" || len(runner.deferred) != 0 {
		t.Fatal("trigger did not combine pending")
	}

	if got := gitOutput(t, runner, "show", "HEAD:"+strings.TrimPrefix(noise, "/")); got != "three" {
		t.Fatal("lost latest deferred bytes", got)
	}

	if message := gitOutput(t, runner, "log", "-1", "--format=%B"); !strings.HasPrefix(message, "unknown:") {
		t.Fatal("mixed commit misattributed", message)
	}
}

func TestNewDeferredAndTriggerPathsKeepTheirSharedActor(t *testing.T) {
	t.Parallel()
	runner, noise, normal := deferFixture(
		t,
		[]filter.Rule{{Paths: []string{noisePattern}, Users: nil, CommandRegex: nil}},
	)
	writeSource(t, runner, "noise", "same actor")
	writeSource(t, runner, "normal", "same actor")

	actor := event.Actor{Known: true, UserKnown: true, UID: 1000, EUID: 1000}
	now := time.Now()

	group := event.Group{Actor: actor, Paths: map[string]event.Raw{
		noise:  {Path: noise, Actor: actor, Time: now},
		normal: {Path: normal, Actor: actor, Time: now},
	}}

	err := runner.commit(t.Context(), group)
	if err != nil {
		t.Fatal(err)
	}

	message := gitOutput(t, runner, "log", "-1", "--format=%B")
	if strings.HasPrefix(message, "unknown:") || strings.Contains(message, "accumulated changes") {
		t.Fatal("single-actor commit was labeled as mixed", message)
	}
}

func TestRevertedDeferredPathDoesNotMakeTriggerMixed(t *testing.T) {
	t.Parallel()
	runner, noise, normal := deferFixture(
		t,
		[]filter.Rule{{Paths: []string{noisePattern}, Users: nil, CommandRegex: nil}},
	)
	writeSource(t, runner, "noise", "pending")
	commitEvent(t, runner, noise, event.Actor{})

	writeSource(t, runner, "noise", "initial")
	writeSource(t, runner, "normal", "trigger")

	actor := event.Actor{Known: true, UserKnown: true, UID: 1000, EUID: 1000}
	now := time.Now()
	group := event.Group{Actor: actor, Paths: map[string]event.Raw{
		noise:  {Path: noise, Actor: actor, Time: now},
		normal: {Path: normal, Actor: actor, Time: now},
	}}

	err := runner.commit(t.Context(), group)
	if err != nil {
		t.Fatal(err)
	}

	message := gitOutput(t, runner, "log", "-1", "--format=%B")
	if strings.HasPrefix(message, "unknown:") || strings.Contains(message, "accumulated changes") {
		t.Fatal("reverted deferred path made an unrelated trigger mixed", message)
	}
}

// Deferred versions live only in the index; superseded blobs must not accumulate while no commit runs.
func TestDeferredRestagingPrunesSupersededVersions(t *testing.T) {
	t.Parallel()

	runner, noise, _ := deferFixture(
		t,
		[]filter.Rule{{Paths: []string{noisePattern}, Users: nil, CommandRegex: nil}},
	)
	index := ":" + strings.TrimPrefix(noise, "/")

	writeSource(t, runner, "noise", "superseded")
	commitEvent(t, runner, noise, event.Actor{})

	superseded := strings.TrimSpace(gitOutput(t, runner, "rev-parse", index))
	runner.lastPrune = time.Time{}

	writeSource(t, runner, "noise", "latest")
	commitEvent(t, runner, noise, event.Actor{})

	if commitCount(t, runner) != "1" || len(runner.deferred) != 1 {
		t.Fatal("deferred change was committed")
	}

	//nolint:gosec // This test uses only its private temporary repository.
	if exec.CommandContext(t.Context(), "git", "-C", runner.repo.Path, "cat-file", "-e", superseded).Run() == nil {
		t.Fatal("superseded deferred blob was not pruned")
	}

	if got := gitOutput(t, runner, "cat-file", "blob", index); got != "latest" {
		t.Fatal("pending deferred version lost", got)
	}
}

func TestUserDeferralDoesNotHideUnknownReplacement(t *testing.T) {
	t.Parallel()
	runner, noise, _ := deferFixture(t, []filter.Rule{{Users: []string{"uid:123"}, Paths: nil, CommandRegex: nil}})
	writeSource(t, runner, "noise", "known writer")
	commitEvent(t, runner, noise, event.Actor{Known: true, UserKnown: true, UID: 123})

	if commitCount(t, runner) != "1" {
		t.Fatal("known user not deferred")
	}

	writeSource(t, runner, "noise", "offline unknown writer")

	err := runner.reconcile(t.Context(), "startup")
	if err != nil {
		t.Fatal(err)
	}

	if commitCount(t, runner) != "2" || len(runner.deferred) != 0 {
		t.Fatal("unknown change inherited old user exclusion")
	}
}

func TestManualFlushAndGitFailureKeepPending(t *testing.T) {
	t.Parallel()
	runner, noise, _ := deferFixture(t, []filter.Rule{{Paths: []string{noisePattern}, Users: nil, CommandRegex: nil}})
	writeSource(t, runner, "noise", "pending")
	commitEvent(t, runner, noise, event.Actor{})

	err := RequestFlush(runner.cfg, runner.name)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.checkFlushRequest()
	if err != nil {
		t.Fatal(err)
	}

	hook := filepath.Join(runner.repo.Path, ".git", "hooks", "pre-commit")

	//nolint:gosec // A private executable Git hook simulates a failed commit.
	err = os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "manual flush")
	if err == nil || len(runner.deferred) != 1 {
		t.Fatal("failure lost pending", err)
	}

	err = os.Remove(hook)
	if err != nil {
		t.Fatal(err)
	}

	runner.flush(t.Context(), time.Now())

	if commitCount(t, runner) != "2" || len(runner.deferred) != 0 || runner.forceCommit {
		t.Fatal("manual flush did not finish")
	}
}

func TestDeferredRenameAndDeleteWaitForTrigger(t *testing.T) {
	t.Parallel()
	runner, noise, normal := deferFixture(
		t,
		[]filter.Rule{{Users: nil, CommandRegex: nil, Paths: []string{"**/noise", "**/renamed"}}},
	)
	renamed := filepath.Join(filepath.Dir(noise), "renamed")

	err := os.Rename(noise, renamed)
	if err != nil {
		t.Fatal(err)
	}

	commitEvent(t, runner, noise, event.Actor{})
	commitEvent(t, runner, renamed, event.Actor{})

	if commitCount(t, runner) != "1" || len(runner.deferred) != 2 {
		t.Fatal("rename was not deferred")
	}

	err = runner.reconcile(t.Context(), "shutdown")
	if err != nil {
		t.Fatal(err)
	}

	if commitCount(t, runner) != "1" {
		t.Fatal("shutdown flushed pending")
	}

	err = os.Remove(renamed)
	if err != nil {
		t.Fatal(err)
	}

	commitEvent(t, runner, renamed, event.Actor{})

	if len(runner.deferred) != 1 {
		t.Fatal("transient creation remained pending")
	}

	writeSource(t, runner, "normal", "trigger")
	commitEvent(t, runner, normal, event.Actor{})

	if commitCount(t, runner) != "2" {
		t.Fatal("deletion was not committed with trigger")
	}

	tree := gitOutput(t, runner, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(tree, "/noise") || strings.Contains(tree, "/renamed") {
		t.Fatal("deleted file survived in Git")
	}
}

func TestChangedRulesReleaseUserDeferredPaths(t *testing.T) {
	t.Parallel()
	runner, noise, _ := deferFixture(t, []filter.Rule{{Users: []string{"uid:123"}, Paths: nil, CommandRegex: nil}})
	writeSource(t, runner, "noise", "pending")
	commitEvent(t, runner, noise, event.Actor{Known: true, UserKnown: true, UID: 123})
	runner.cfg.Commit.Defer = nil

	err := runner.loadDeferral(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "changed policy")
	if err != nil {
		t.Fatal(err)
	}

	if commitCount(t, runner) != "2" || len(runner.deferred) != 0 {
		t.Fatal("disabled rule kept changes deferred")
	}
}
