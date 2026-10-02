package catalog

import (
	"reflect"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

func repositoryConfiguration(t *testing.T) *config.Config {
	t.Helper()

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	repo := cfg.Repositories["repo"]
	repo.Type, repo.Storage.Path = config.RepositoryDB, t.TempDir()
	repo.Integrity.Hash.Algorithm = config.HashSHA512
	repo.Notifications.Use = []string{fixtureDestinationFirst}
	repo.Reports = reportSettings()
	cfg.Repositories["repo"] = repo
	cfg.Notifiers = map[string]config.Notifier{
		fixtureDestinationFirst: {
			Type: config.NotifierEmail, DSN: "smtp://localhost", From: "sender@example.invalid",
			To: []string{"recipient@example.invalid"}, Template: "", URL: "",
		},
	}

	return cfg
}

func TestOpenRepositoryUsesOverridesAndTracksRestart(t *testing.T) {
	t.Parallel()

	cfg := repositoryConfiguration(t)
	repo := cfg.Repositories["repo"]

	for index, effective := range []*config.Config{cfg, cfg.ForRepository("repo")} {
		func() {
			store, err := OpenRepository(t.Context(), effective, "repo")
			if err != nil {
				t.Fatal(err)
			}
			defer resource.Close(store)

			if !reflect.DeepEqual(store.Policy, repo.Integrity) ||
				!reflect.DeepEqual(store.Notifications, repo.Notifications) ||
				!reflect.DeepEqual(store.reports, repo.Reports) {
				t.Fatal("catalog did not use the repository overrides")
			}

			if store.versions[fixtureDestinationFirst] != config.NotifierVersion(
				cfg.Notifiers[fixtureDestinationFirst],
			) {
				t.Fatal("notification destination was not bound")
			}

			if store.Reports().CoverageGap != (index > 0) {
				t.Fatal("restart coverage gap was not preserved")
			}
		}()
	}
}

func TestOpenRepositoryClosesCatalogOnReportFailure(t *testing.T) {
	t.Parallel()

	cfg := repositoryConfiguration(t)
	repo := cfg.Repositories["repo"]
	repo.Reports.Schedule = "invalid schedule"
	cfg.Repositories["repo"] = repo

	store, err := OpenRepository(t.Context(), cfg, "repo")
	if err == nil || store != nil {
		t.Fatal("invalid report schedule opened a catalog", err)
	}

	repo.Reports = reportSettings()
	cfg.Repositories["repo"] = repo

	store, err = OpenRepository(t.Context(), cfg, "repo")
	if err != nil {
		t.Fatal("failed startup retained the catalog lock", err)
	}

	resource.Close(store)
}
