package grouping

import (
	"time"

	"github.com/inode64/fsledger/internal/event"
)

type pendingEnrichment struct {
	first time.Time
	last  time.Time
	raw   event.Raw
}

// Enrich retries delayed attribution before flushing, without extending debounce/max-delay.
func (m *Manager) Enrich(resolve func(event.Raw) event.Actor) {
	var enriched []pendingEnrichment

	for key, group := range m.groups {
		if group.Actor.Known || group.Reason == conflictingActors {
			continue
		}

		enriched = append(enriched, takeEnriched(group, resolve)...)

		if len(group.Paths) == 0 {
			delete(m.groups, key)
		}
	}

	for _, value := range enriched {
		m.paths.Delete(value.raw.Path)
	}

	for _, value := range enriched {
		group := m.add(value.raw)

		group.First = minTime(group.First, value.first)
		if value.last.After(group.Last) {
			group.Last = value.last
		}
	}
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}

	return second
}

func takeEnriched(group *event.Group, resolve func(event.Raw) event.Actor) []pendingEnrichment {
	var enriched []pendingEnrichment

	for path, raw := range group.Paths {
		if raw.Actor.Known {
			continue
		}

		actor := resolve(raw)
		if !actor.Known {
			continue
		}

		raw.Actor = actor
		enriched = append(enriched, pendingEnrichment{raw: raw, first: group.First, last: group.Last})

		delete(group.Paths, path)
	}

	return enriched
}
