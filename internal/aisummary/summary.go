package aisummary

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aymanbagabas/go-udiff"
	"github.com/bmatcuk/doublestar/v4"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/redact"
)

const (
	statusTimeout     = "timeout"
	statusUnavailable = "unavailable"
	diffHeaderBudget  = 128
	maxConcurrent     = 2
	maxFiles          = 32
	maxLines          = 1024
	diffContext       = 3
	cooldown          = time.Minute
)

// Manager bounds outbound concurrency across all repositories in one daemon.
type Manager struct {
	slots    chan struct{}
	profiles map[string]config.AIProfile
}

// Session holds one worker's includes, redaction policy and provider cooldowns.
type Session struct {
	masker    *redact.Masker
	manager   *Manager
	include   []string
	cooldowns map[string]time.Time
	names     []string
}

// New creates an isolated concurrency budget without making network requests.
func New(profiles map[string]config.AIProfile) *Manager {
	return &Manager{slots: make(chan struct{}, maxConcurrent), profiles: profiles}
}

// Session compiles an enabled repository selection after configuration validation.
func (manager *Manager) Session(repo *config.Repository) (*Session, error) {
	if repo == nil {
		return nil, fault.New("AI repository selection missing")
	}

	include, err := selection(repo.IAInclude)
	if err != nil {
		return nil, err
	}

	return manager.session(repo.IACommit, include)
}

// selection validates an allowlist and normalizes it as exclusions are, so it matches what validation accepted.
func selection(patterns []string) ([]string, error) {
	err := config.ValidateAIIncludes(patterns)
	if err != nil {
		return nil, err
	}

	include := make([]string, 0, len(patterns))

	for _, pattern := range patterns {
		selected, err := exclude.Selection(pattern)
		if err != nil {
			return nil, err
		}

		include = append(include, selected)
	}

	return include, nil
}

func (manager *Manager) session(names, include []string) (*Session, error) {
	var rules []redact.Rule
	for _, name := range names {
		rules = append(rules, manager.profiles[name].Redact...)
	}

	masker, err := redact.Compile(rules)
	if err != nil {
		return nil, err
	}

	return &Session{
		masker:    masker,
		manager:   manager,
		names:     names,
		include:   include,
		cooldowns: make(map[string]time.Time),
	}, nil
}

// Summarize is called only for a real commit, by the owning serialized worker.
// All providers share a five-second total deadline; an occupied budget skips AI.
func (session *Session) Summarize(
	ctx context.Context,
	repo *gitrepo.Repository,
	versions map[string]string,
) (string, string) {
	// Blobs read and masked now could not be sent, and would hold a slot other repositories need.
	if session.coolingDown() {
		return "", statusUnavailable
	}

	ctx, release := session.acquire(ctx)
	if release == nil {
		return "", "busy"
	}
	defer release()

	// Patches depend on the staged blobs and the shared masker, not on the provider: a fallback reuses them.
	patches, err := session.prepare(ctx, repo, versions)
	if err != nil {
		if ctx.Err() != nil {
			return "", statusTimeout
		}

		return "", statusUnavailable
	}

	return session.tryProviders(ctx, func(ctx context.Context, name string) (string, string, error) {
		return session.attempt(ctx, name, patches)
	})
}

func (session *Session) attempt(
	ctx context.Context,
	name string,
	patches *preparedPatches,
) (string, string, error) {
	profile := session.manager.profiles[name]

	bounded := patches.bounded(profile.MaxDiffBytes)
	prompt := bounded.result()

	if prompt == "" {
		return "", "no-visible-diff", nil
	}

	if bounded.output.Len() == 0 {
		if bounded.redacted == bounded.omitted {
			return "Resumen local: cambian valores ocultos por la política de IA.", "redacted", nil
		}

		return fmt.Sprintf("Resumen local: %d archivos autorizados omitidos por límites o política de IA.",
			bounded.omitted), "omitted", nil
	}

	text, err := providerSummary(ctx, profile, instruction, prompt)
	if err != nil {
		return "", "", err
	}

	return "Resumen generado por IA (" + name + "):\n" + text, "summarized:" + name, nil
}

type preparedPatch struct {
	text   string
	masked bool
}

// preparedPatches are the masked patches of one commit, before any provider's size limit.
type preparedPatches struct {
	patches []preparedPatch
	omitted int
}

func (session *Session) prepare(
	ctx context.Context,
	repo *gitrepo.Repository,
	versions map[string]string,
) (*preparedPatches, error) {
	paths := make([]string, 0, len(versions))
	for path := range versions {
		if session.selected(path) {
			paths = append(paths, path)
		}
	}

	sort.Strings(paths)

	prepared := &preparedPatches{patches: nil, omitted: max(0, len(paths)-maxFiles)}

	for _, path := range paths[:min(len(paths), maxFiles)] {
		if ctx.Err() != nil {
			return nil, fault.Wrap("prepare AI diff", ctx.Err())
		}

		// Mask complete raw names before quoting, as reports do; a name that cannot be masked is omitted.
		name, err := session.masker.Apply(path)
		if err != nil {
			prepared.omitted++

			continue
		}

		patch, masked, unchanged, err := maskedPatch(ctx, repo, name, versions[path], session.masker)
		if err != nil {
			return nil, err
		}

		// Identical text has no diff to summarize, so it is not an omission either.
		if unchanged {
			continue
		}

		prepared.patches = append(prepared.patches, preparedPatch{text: patch, masked: masked})
	}

	return prepared, nil
}

func (prepared *preparedPatches) bounded(limit int) *patchBuilder {
	builder := patchBuilder{
		limit: max(0, limit-diffHeaderBudget), output: strings.Builder{}, omitted: prepared.omitted, redacted: 0,
	}
	for _, patch := range prepared.patches {
		builder.add(patch.text, patch.masked)
	}

	return &builder
}

type patchBuilder struct {
	output   strings.Builder
	limit    int
	omitted  int
	redacted int
}

func (builder *patchBuilder) add(patch string, masked bool) {
	if patch == "" && masked {
		builder.omitted++
		builder.redacted++

		return
	}

	if patch == "" || builder.output.Len()+len(patch) > builder.limit {
		builder.omitted++

		return
	}

	builder.output.WriteString(patch) //nolint:errcheck // strings.Builder.WriteString always returns nil.
}

func (builder *patchBuilder) result() string {
	if builder.output.Len() == 0 && builder.omitted == 0 {
		return ""
	}

	header := fmt.Sprintf(
		"Diff parcial de rutas autorizadas. %d archivos autorizados omitidos; no deducir su contenido.\n",
		builder.omitted,
	)

	return header + builder.output.String()
}

// maskedPatch labels the diff with path as given: callers pass a name already masked.
// Identical text, such as after a mode-only change, is reported as unchanged.
func maskedPatch(
	ctx context.Context,
	repo *gitrepo.Repository,
	path, signature string,
	masker *redact.Masker,
) (string, bool, bool, error) {
	before, after, eligible, err := repo.StagedText(ctx, signature)
	if err != nil || !eligible {
		return "", false, false, err
	}

	if before == after {
		return "", false, true, nil
	}

	oldText, err := masker.Apply(before)
	if err != nil {
		return "", false, false, err
	}

	newText, err := masker.Apply(after)
	if err != nil {
		return "", false, false, err
	}

	if strings.Count(oldText, "\n") > maxLines || strings.Count(newText, "\n") > maxLines {
		return "", false, false, nil
	}

	if oldText == newText {
		return "", true, false, nil
	}

	patch, err := udiff.ToUnified(
		fmt.Sprintf("%q (before)", path),
		fmt.Sprintf("%q (after)", path),
		oldText, udiff.Lines(oldText, newText), diffContext,
	)
	if err != nil {
		return "", false, false, fault.New("cannot prepare masked AI diff")
	}

	return patch, false, false, nil
}

func (session *Session) selected(path string) bool {
	for _, pattern := range session.include {
		matched, err := doublestar.Match(pattern, path)
		if err == nil && matched {
			return true
		}
	}

	return false
}
