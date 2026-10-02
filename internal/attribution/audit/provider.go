package audit

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/elastic/go-libaudit/v2"
	"github.com/elastic/go-libaudit/v2/auparse"
	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	matchWindow         = 250 * time.Millisecond
	retention           = 2 * time.Second
	maximumObservations = 4096
	readInterval        = 10 * time.Millisecond
	readBudget          = 512
)

type observation struct {
	time      time.Time
	path      string
	operation string
	keys      []string
	actor     event.Actor
	device    uint64
	inode     uint64
}

// Provider receives passive multicast records and resolves only recent, unambiguous matches.
// Linux library types stay inside this adapter. It never installs rules or claims the controller.
type Provider struct {
	blockedUntil time.Time
	client       *libaudit.AuditClient
	reassembler  *libaudit.Reassembler
	logger       *slog.Logger
	scopes       map[string]func(string) bool
	cancel       context.CancelFunc
	done         chan struct{}
	observations []observation
	once         sync.Once
	mu           sync.Mutex
}

// Start opens a passive subscriber. Absence of Audit is an ordinary fallback error.
func Start(ctx context.Context, scopes map[string]func(string) bool, logger *slog.Logger) (*Provider, error) {
	client, err := libaudit.NewMulticastAuditClient(nil)
	if err != nil {
		return nil, fault.Wrap("subscribe to Audit multicast", err)
	}

	provider, err := newProvider(scopes, logger)
	if err != nil {
		resource.Close(client)

		return nil, err
	}

	provider.client = client

	ctx, provider.cancel = context.WithCancel(ctx)
	go provider.read(ctx)

	return provider, nil
}

// Resolve preserves uncertainty when competing actors or missing records prevent correlation.
func (provider *Provider) Resolve(ctx context.Context, raw event.Raw, key string) event.Actor {
	if ctx.Err() != nil {
		return event.Actor{}
	}

	if accept := provider.scopes[key]; accept == nil || !accept(raw.Path) {
		return event.Actor{}
	}

	now := time.Now()
	if now.Sub(raw.Time) < matchWindow || now.Sub(raw.Time) > retention {
		return event.Actor{}
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()

	if now.Before(provider.blockedUntil) {
		return event.Actor{}
	}

	var result event.Actor

	for _, candidate := range provider.observations {
		if !slices.Contains(candidate.keys, key) || !matches(candidate, raw) || !objectMatches(candidate) {
			continue
		}

		if result.Known && !sameActor(result, candidate.actor) {
			return event.Actor{}
		}

		result = candidate.actor
	}

	return result
}

func sameActor(first, second event.Actor) bool {
	if first.Evidence != "" && first.Evidence == second.Evidence {
		return true
	}

	return first.StartTime != 0 && first.StartTime == second.StartTime && first.PID == second.PID &&
		first.UID == second.UID && first.EUID == second.EUID && first.LoginUID == second.LoginUID
}

// Close stops reception before closing the socket and discards unfinished records.
func (provider *Provider) Close() error {
	var result error

	provider.once.Do(func() {
		if provider.cancel != nil {
			provider.cancel()
			<-provider.done
		}

		if provider.client != nil {
			result = provider.client.Close()
		}
		// Invalidate before flushing: incomplete records cannot become fresh attribution.
		provider.invalidate("Audit subscription closed")
		result = errors.Join(result, provider.reassembler.Close())
	})

	return fault.Wrap("close Audit subscriber", result)
}

// ReassemblyComplete implements libaudit's stream callback within the adapter.
func (provider *Provider) ReassemblyComplete(messages []*auparse.AuditMessage) {
	values, err := coalesce(messages, provider.scopes)
	if err != nil {
		provider.logger.Debug("Audit record rejected", "reason", err.Error())

		return
	}

	now := time.Now()

	provider.mu.Lock()
	defer provider.mu.Unlock()

	for _, value := range values {
		if now.Sub(value.time) > retention || value.time.After(now.Add(matchWindow)) || !provider.accepts(value) {
			continue
		}

		if len(provider.observations) >= maximumObservations {
			provider.observations = nil
			provider.blockedUntil = now.Add(retention)
			provider.logger.Warn("Audit attribution queue overflow; filesystem detection continues")

			return
		}

		provider.observations = append(provider.observations, value)
	}
}

// EventsLost invalidates correlation after the reassembler detects a sequence gap.
func (provider *Provider) EventsLost(count int) {
	provider.logger.Warn("Audit events lost", "count", count)
	provider.invalidate("Audit sequence gap")
}

func newProvider(scopes map[string]func(string) bool, logger *slog.Logger) (*Provider, error) {
	provider := &Provider{
		scopes: scopes,
		logger: logger,
		done:   make(chan struct{}),
	}

	assembler, err := libaudit.NewReassembler(maximumObservations, matchWindow, provider)
	if err != nil {
		return nil, fault.Wrap("create Audit reassembler", err)
	}

	provider.reassembler = assembler

	return provider, nil
}

func matches(candidate observation, raw event.Raw) bool {
	delta := candidate.time.Sub(raw.Time)

	return candidate.path == raw.Path && delta >= -matchWindow && delta <= matchWindow &&
		(raw.PID <= 0 || candidate.actor.PID == raw.PID) &&
		(candidate.operation == raw.Operation || (candidate.operation == event.Create && raw.Operation == event.Write) ||
			(raw.Operation == event.Rename && (candidate.operation == event.Create || candidate.operation == event.Remove)))
}

func (provider *Provider) invalidate(reason string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	provider.observations = nil
	provider.blockedUntil = time.Now().Add(retention)
	provider.logger.Debug("Audit correlation invalidated", "reason", reason)
}

func (provider *Provider) read(ctx context.Context) {
	defer close(provider.done)

	ticker := time.NewTicker(readInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !provider.receive() {
				return
			}

			err := provider.reassembler.Maintain()
			if err != nil {
				provider.invalidate(err.Error())

				return
			}

			provider.prune()
		}
	}
}

func (provider *Provider) receive() bool {
	for range readBudget {
		message, err := provider.client.Receive(true)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			return true
		}

		if err != nil {
			provider.invalidate(err.Error())
			provider.logger.Warn("Audit receive failed; attribution unavailable", "error", err)

			return errors.Is(err, unix.ENOBUFS)
		}

		err = provider.reassembler.Push(message.Type, message.Data)
		if err != nil {
			provider.invalidate(err.Error())
		}
	}

	return true
}

func (provider *Provider) prune() {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	cutoff := time.Now().Add(-retention)

	kept := provider.observations[:0]
	for _, value := range provider.observations {
		if !value.time.Before(cutoff) {
			kept = append(kept, value)
		}
	}

	provider.observations = kept
}

func (provider *Provider) accepts(value observation) bool {
	for _, key := range value.keys {
		if accept := provider.scopes[key]; accept != nil && accept(value.path) {
			return true
		}
	}

	return false
}
