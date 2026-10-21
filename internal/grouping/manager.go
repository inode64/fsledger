// Package grouping batches observed actors without conflating reused process IDs.
package grouping

import (
	"fmt"
	"sort"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/pathindex"
)

const conflictingActors = "conflicting actors"

// Manager is owned by a single repository event loop.
type Manager struct {
	groups    map[string]*event.Group
	paths     pathindex.Index[string]
	policy    config.Grouping
	debounce  time.Duration
	timeLimit time.Duration
}

// New creates an empty repository-local grouping manager.
func New(policy config.Grouping, debounce, maxDelay time.Duration) *Manager {
	return &Manager{
		paths:     pathindex.Index[string]{},
		policy:    policy,
		debounce:  debounce,
		timeLimit: maxDelay,
		groups:    make(map[string]*event.Group),
	}
}

// Add places an event in its actor's group. Overlapping paths from distinct
// actors are downgraded to unknown because the filesystem retains only one value.
func (m *Manager) Add(raw event.Raw) { m.add(raw) }

// Clear discards all pending groups and their path memberships.
func (m *Manager) Clear() {
	clear(m.groups)
	m.paths = pathindex.Index[string]{}
}

// DueWithAttributionDelay retains unresolved events until delayed attribution can run.
// Explicit draining still takes precedence during reconciliation and shutdown.
func (m *Manager) DueWithAttributionDelay(now time.Time, all bool, delay time.Duration) []event.Group {
	var result []event.Group

	for key, group := range m.groups {
		if all || now.Sub(group.First) >= max(m.timeLimit, delay) ||
			(now.Sub(group.Last) >= m.debounce && attributionReady(group, now, delay)) {
			result = append(result, *group)
			for path := range group.Paths {
				m.paths.Delete(path)
			}

			delete(m.groups, key)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].First.Equal(result[j].First) {
			return firstPath(result[i]) < firstPath(result[j])
		}

		return result[i].First.Before(result[j].First)
	})

	return result
}

func attributionReady(group *event.Group, now time.Time, delay time.Duration) bool {
	if delay <= 0 || group.Actor.Known || group.Reason == conflictingActors {
		return true
	}

	for _, raw := range group.Paths {
		if !raw.Actor.Known && now.Sub(raw.Time) < delay {
			return false
		}
	}

	return true
}

func firstPath(group event.Group) string {
	first := ""
	for path := range group.Paths {
		if first == "" || path < first {
			first = path
		}
	}

	return first
}

// Pending reports groups and unique path counts.
func (m *Manager) Pending() (int, int) {
	paths := 0
	for _, group := range m.groups {
		paths += len(group.Paths)
	}

	return len(m.groups), paths
}

func (m *Manager) add(raw event.Raw) *event.Group {
	if raw.Time.IsZero() {
		raw.Time = time.Now()
	}

	key := m.key(raw.Actor)

	collisions := m.collect(raw, key)
	if collisions.duplicate {
		return collisions.group
	}

	first, moved, conflict := collisions.first, collisions.moved, collisions.conflict

	if conflict {
		key = event.Unknown
		raw.Actor = event.Actor{}
		raw.Reason = conflictingActors
	}

	group := m.groups[key]
	if group == nil {
		group = &event.Group{Actor: m.groupActor(raw.Actor), Paths: make(map[string]event.Raw), First: first}
		m.groups[key] = group
	}

	if key == event.Unknown {
		group.Actor = event.Actor{}
	}

	for path, previous := range moved {
		previous.Actor = event.Actor{}
		group.Paths[path] = previous
		m.paths.Set(path, key)
	}

	group.Paths[raw.Path] = raw
	m.paths.Set(raw.Path, key)

	if raw.Time.After(group.Last) {
		group.Last = raw.Time
	}

	if first.Before(group.First) {
		group.First = first
	}

	if raw.Reason != "" {
		group.Reason = raw.Reason
	}

	return group
}

type collisions struct {
	first     time.Time
	moved     map[string]event.Raw
	group     *event.Group
	conflict  bool
	duplicate bool
}

func sameWrite(previous, raw event.Raw) bool {
	const duplicateWindow = 250 * time.Millisecond

	delta := previous.Time.Sub(raw.Time)
	if previous.Path != raw.Path || delta < -duplicateWindow || delta > duplicateWindow {
		return false
	}

	if previous.Operation == raw.Operation {
		return true
	}

	if (previous.Operation == event.Write || previous.Operation == event.Create) &&
		(raw.Operation == event.Write || raw.Operation == event.Create) {
		return true
	}

	return (previous.Operation == event.Rename && (raw.Operation == event.Create || raw.Operation == event.Remove)) ||
		(raw.Operation == event.Rename && (previous.Operation == event.Create || previous.Operation == event.Remove))
}

func knownDuplicate(group *event.Group, previous, raw event.Raw) bool {
	return sameWrite(previous, raw) && group.Actor.Known && raw.Backend == event.Inotify &&
		previous.Backend == event.Fanotify && !createAfterWrite(previous, raw)
}

// createAfterWrite reports an inotify creation seen after the attributed write. Without fanotify namespace
// events it is another process replacing the path, such as a rename over it, not the writer's own creation.
func createAfterWrite(previous, raw event.Raw) bool {
	return raw.Operation == event.Create && previous.Operation == event.Write && raw.Time.After(previous.Time)
}

func canEnrich(group *event.Group, previous, raw event.Raw) bool {
	return sameWrite(previous, raw) && raw.Actor.Known && raw.Backend == event.Fanotify &&
		previous.Backend == event.Inotify &&
		group.Reason != conflictingActors
}

func (m *Manager) key(actor event.Actor) string {
	if !m.policy.ByProcess && !m.policy.ByUser {
		return event.Unknown
	}

	if !actor.Known || (m.policy.ByProcess && (actor.PID <= 0 || (actor.StartTime == 0 && actor.Evidence == ""))) {
		return event.Unknown
	}

	key := "known"
	if m.policy.ByProcess {
		key += processKey(actor)
	}

	if m.policy.ByUser {
		if !actor.UserKnown {
			return event.Unknown
		}

		uid := actor.UID
		if m.policy.PreferLoginUID && actor.LoginKnown {
			uid = actor.LoginUID
		}

		key += fmt.Sprintf("/uid:%d/euid:%d", uid, actor.EUID)
	}

	return key
}

func (m *Manager) groupActor(actor event.Actor) event.Actor {
	if !m.policy.ByProcess {
		actor.Evidence = ""
		actor.PID = 0
		actor.StartTime = 0
		actor.Executable = ""
		actor.Command = ""
	}

	if !m.policy.ByUser {
		actor.UserKnown = false
		actor.LoginKnown = false
		actor.UID = 0
		actor.EUID = 0
		actor.LoginUID = 0
	}

	return actor
}

func (m *Manager) collect(raw event.Raw, key string) *collisions {
	result := &collisions{
		first:     raw.Time,
		moved:     make(map[string]event.Raw),
		conflict:  false,
		duplicate: false,
		group:     nil,
	}

	if group := m.duplicate(raw, key); group != nil {
		if raw.Time.After(group.Last) {
			group.Last = raw.Time
		}

		result.duplicate, result.group = true, group

		return result
	}

	var own []string

	for path, existing := range m.paths.Overlaps(raw.Path) {
		group := m.groups[existing]
		if group == nil {
			result.conflict = true

			m.paths.Delete(path)

			continue
		}

		if existing == key {
			own = append(own, path)

			continue
		}

		result.inspect(group, path, raw)
		m.paths.Delete(path)

		if len(group.Paths) == 0 {
			delete(m.groups, existing)
		}
	}

	m.release(result, key, own)

	return result
}

// release moves the actor's own overlapping entries along with a conflicting
// event, so a path never stays in two groups once the event becomes unknown.
func (m *Manager) release(result *collisions, key string, paths []string) {
	if !result.conflict || key == event.Unknown || len(paths) == 0 {
		return
	}

	group := m.groups[key]
	if group == nil {
		return
	}

	for _, path := range paths {
		result.moved[path] = group.Paths[path]
		delete(group.Paths, path)
		m.paths.Delete(path)
	}

	if group.First.Before(result.first) {
		result.first = group.First
	}

	if len(group.Paths) == 0 {
		delete(m.groups, key)
	}
}

func (c *collisions) inspect(group *event.Group, path string, raw event.Raw) {
	previous := group.Paths[path]
	if !canEnrich(group, previous, raw) {
		c.conflict = true
		c.moved[path] = previous
	}

	if group.First.Before(c.first) {
		c.first = group.First
	}

	delete(group.Paths, path)
}

func processKey(actor event.Actor) string {
	key := fmt.Sprintf("/pid:%d/start:%d", actor.PID, actor.StartTime)
	if actor.StartTime == 0 {
		key += "/evidence:" + actor.Evidence
	}

	return key
}

func (m *Manager) duplicate(raw event.Raw, key string) *event.Group {
	existing, found := m.paths.Get(raw.Path)
	if !found || existing == key {
		return nil
	}

	group := m.groups[existing]
	if group == nil {
		return nil
	}

	if knownDuplicate(group, group.Paths[raw.Path], raw) {
		return group
	}

	return nil
}
