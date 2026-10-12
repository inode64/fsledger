package cli

import (
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	gitrepo "github.com/inode64/fsledger/internal/git"
)

// selectRepository resolves the --repository operand of per-repository commands.
func selectRepository(cfg *config.Config, name string) (config.Repository, error) {
	repo, exists := cfg.Repositories[name]
	if !exists {
		return config.Repository{}, fault.New("select an existing repository with --repository")
	}

	return repo, nil
}

// gitBackend addresses the Git mirror of a configured repository without touching it.
func gitBackend(cfg *config.Config, name string) *gitrepo.Repository {
	return &gitrepo.Repository{
		Path:    cfg.RepositoryPath(name),
		Host:    cfg.Server.Name,
		Timeout: cfg.Repositories[name].Storage.Git.Timeout,
	}
}
