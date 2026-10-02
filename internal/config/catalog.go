package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const maxConfigFiles = 256

type catalogLoader struct {
	seen  map[string]bool
	bytes int
}

// Load reads named catalogs, resolves inheritance, then validates the complete configuration.
func Load(path string) (*Config, error) {
	cfg := defaults()

	loader := catalogLoader{seen: make(map[string]bool), bytes: 0}

	file, err := loader.open(path)
	if err != nil {
		return nil, err
	}
	defer resource.Close(file)

	err = loader.decode(file, cfg)
	if err != nil {
		return nil, err
	}

	cfg.Repositories = make(map[string]Repository)
	cfg.Notifiers = make(map[string]Notifier)
	cfg.AI = make(map[string]AIProfile)

	cfg.Templates = make(map[string]Template)

	err = loader.objects(filepath.Dir(file.Name()), cfg)
	if err != nil {
		return nil, err
	}

	err = cfg.Validate()
	if err != nil {
		return nil, fault.Wrap("validate configuration", err)
	}

	return cfg, nil
}

func (loader *catalogLoader) objects(base string, cfg *Config) error {
	profiles := make(map[string][]string)

	err := loader.catalog(base, cfg.Paths.Excludes, func(name, file string) error {
		if _, exists := profiles[name]; exists {
			return fault.New("duplicate exclusion profile: " + name)
		}

		var patterns []string

		readErr := loader.read(file, &patterns)
		if readErr != nil {
			return readErr
		}

		profiles[name] = patterns

		return nil
	})
	if err != nil {
		return err
	}

	cfg.Exclude, err = expandProfiles(cfg.Exclude, profiles)
	if err != nil {
		return err
	}

	err = loader.repositories(base, cfg, profiles)
	if err != nil {
		return err
	}

	err = loader.aiProfiles(base, cfg)
	if err != nil {
		return err
	}

	err = loader.notifiers(base, cfg)
	if err != nil {
		return err
	}

	if cfg.Paths.Templates == "" {
		return nil
	}

	if !filepath.IsAbs(cfg.Paths.Templates) {
		cfg.Paths.Templates = filepath.Join(base, cfg.Paths.Templates)
	}

	return loader.templates(cfg)
}

func (loader *catalogLoader) repositories(base string, cfg *Config, profiles map[string][]string) error {
	return loader.catalog(base, cfg.Paths.Repositories, func(name, file string) error {
		if _, exists := cfg.Repositories[name]; exists {
			return fault.New("duplicate repository: " + name)
		}

		repo := cfg.repositoryDefaults()

		err := loader.read(file, &repo)
		if err != nil {
			return err
		}

		repo.Exclude, err = expandProfiles(repo.Exclude, profiles)
		if err != nil {
			return err
		}

		cfg.Repositories[name] = repo

		return nil
	})
}

func (loader *catalogLoader) notifiers(base string, cfg *Config) error {
	return loader.catalog(base, cfg.Paths.Notifiers, func(name, file string) error {
		if _, exists := cfg.Notifiers[name]; exists {
			return fault.New("duplicate notifier: " + name)
		}

		var notifier Notifier

		err := loader.read(file, &notifier)
		if err != nil {
			return fault.New("invalid notifier document: " + file)
		}

		cfg.Notifiers[name] = notifier

		return nil
	})
}

func expandProfiles(names []string, profiles map[string][]string) ([]string, error) {
	var patterns []string

	for _, name := range names {
		profile, ok := profiles[name]
		if !ok {
			return nil, fault.New("unknown exclusion profile: " + name)
		}

		patterns = append(patterns, profile...)
	}

	return patterns, nil
}

func configurationPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fault.Wrap("absolute configuration path", err)
	}

	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve configuration %q: %w", path, err)
	}

	return canonical, nil
}

func objectName(path string) string {
	name := filepath.Base(path)

	extension := filepath.Ext(name)
	if extension == ".yaml" || extension == ".yml" {
		name = strings.TrimSuffix(name, extension)
	}

	return name
}

func (loader *catalogLoader) catalog(base string, paths []string, visit func(string, string) error) error {
	for _, path := range paths {
		if path == "" {
			return fault.New("catalog path must not be empty")
		}

		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}

		info, err := os.Stat(path)
		if err != nil {
			return fault.Wrap("inspect catalog", err)
		}

		if !info.IsDir() {
			err = visit(objectName(path), path)
			if err != nil {
				return err
			}

			continue
		}

		err = loader.directory(path, visit)
		if err != nil {
			return err
		}
	}

	return nil
}

func (loader *catalogLoader) open(path string) (*os.File, error) {
	canonical, err := configurationPath(path)
	if err != nil {
		return nil, err
	}

	if loader.seen[canonical] {
		return nil, fault.New("repeated configuration file: " + path)
	}

	if len(loader.seen) >= maxConfigFiles {
		return nil, fault.New("configuration exceeds 256 files")
	}

	loader.seen[canonical] = true

	fd, err := unix.Open(canonical, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fault.Wrap("open configuration", err)
	}

	file := os.NewFile(uintptr(fd), canonical)

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		resource.Close(file)

		return nil, fault.New("configuration must be a regular file: " + path)
	}

	return file, nil
}

func (loader *catalogLoader) read(path string, target any) error {
	file, err := loader.open(path)
	if err != nil {
		return err
	}
	defer resource.Close(file)

	return loader.decode(file, target)
}

func (loader *catalogLoader) decode(file *os.File, target any) error {
	size, err := decodeDocument(file, target, maxConfigBytes-loader.bytes)
	loader.bytes += size

	if err != nil {
		return fmt.Errorf("configuration %q: %w", file.Name(), err)
	}

	return nil
}

func (*catalogLoader) directory(path string, visit func(string, string) error) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fault.Wrap("read catalog", err)
	}

	for _, entry := range entries {
		extension := filepath.Ext(entry.Name())
		if entry.IsDir() || (extension != ".yaml" && extension != ".yml") {
			continue
		}

		err = visit(objectName(entry.Name()), filepath.Join(path, entry.Name()))
		if err != nil {
			return err
		}
	}

	return nil
}
