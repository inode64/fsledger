// Package event defines backend-independent filesystem events and actors.
package event

import (
	"os"
	"time"
)

// Backend, operation and reconciliation names are stable across adapters and commit messages.
const (
	Unknown        = "unknown"
	Fanotify       = "fanotify"
	Inotify        = "inotify"
	Polling        = "polling"
	Reconciliation = "reconciliation"
	Write          = "WRITE"
	Create         = "CREATE"
	Remove         = "REMOVE"
	Rename         = "RENAME"
	Attrib         = "ATTRIB"
)

// Actor records only identity that was actually observed.
type Actor struct {
	Evidence          string `json:"evidence,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Executable        string `json:"executable"`
	// Command is ephemeral evidence for local command_regex filters; arguments may contain credentials.
	Command    string `json:"-"`
	PID        int    `json:"pid"`
	StartTime  uint64 `json:"start_time"`
	UID        uint32 `json:"uid"`
	EUID       uint32 `json:"euid"`
	LoginUID   uint32 `json:"login_uid"`
	UserKnown  bool   `json:"user_known"`
	LoginKnown bool   `json:"login_known"`
	Known      bool   `json:"known"`
}

// Raw is emitted without performing mirror or Git work.
type Raw struct {
	Time      time.Time
	PIDFD     *os.File
	Path      string
	OldPath   string
	Operation string
	Backend   string
	Reason    string
	Actor     Actor
	PID       int
	Dirty     bool
}

// Group is an isolated set of pending paths owned by one observed actor.
type Group struct {
	First  time.Time
	Last   time.Time
	Paths  map[string]Raw
	Reason string
	Actor  Actor
}
