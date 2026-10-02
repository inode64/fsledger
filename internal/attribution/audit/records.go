package audit

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/elastic/go-libaudit/v2/aucoalesce"
	"github.com/elastic/go-libaudit/v2/auparse"
	"golang.org/x/sys/unix"

	procattr "github.com/inode64/fsledger/internal/attribution/proc"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/fault"
)

func coalesce(messages []*auparse.AuditMessage, scopes map[string]func(string) bool) ([]observation, error) {
	err := complete(messages)
	if err != nil {
		return nil, err
	}

	value, err := aucoalesce.CoalesceMessages(messages)
	if err != nil {
		return nil, fault.Wrap("coalesce Audit records", err)
	}

	if value.Result != "success" ||
		!slices.ContainsFunc(value.Tags, func(key string) bool { return scopes[key] != nil }) ||
		len(value.Warnings) > 0 {
		return nil, nil
	}

	operationCode := auditOperation(value.Data)
	if operationCode == "" {
		return nil, nil
	}

	actor, err := recordedActor(value)
	if err != nil {
		return nil, err
	}

	return recordedPaths(value, actor, operationCode), nil
}

func recordedPaths(value *aucoalesce.Event, actor event.Actor, operationCode string) []observation {
	var result []observation

	for _, file := range value.Paths {
		name := file["name"]
		if name == "" || name == "(null)" || file["nametype"] == "PARENT" {
			continue
		}

		name = recordedPath(name, value)
		if name == "" {
			continue
		}

		operation := operationCode
		if file["nametype"] == "DELETE" {
			operation = event.Remove
		}

		if file["nametype"] == event.Create {
			operation = event.Create
		}

		device, inode := fileIdentity(file)
		result = append(
			result,
			observation{
				time:      value.Timestamp,
				keys:      value.Tags,
				actor:     actor,
				path:      name,
				operation: operation,
				device:    device,
				inode:     inode,
			},
		)
	}

	return result
}

func complete(messages []*auparse.AuditMessage) error {
	err := validateTransaction(messages)
	if err != nil {
		return err
	}

	var syscalls, expected int

	finished := false
	items := make(map[string]bool)

	for _, message := range messages {
		data, err := message.Data()
		if err != nil && message.RecordType != auparse.AUDIT_EOE {
			return fault.Wrap("parse Audit fields", err)
		}
		//nolint:exhaustive // Only the records that establish complete filesystem syscalls participate in attribution.
		switch message.RecordType {
		case auparse.AUDIT_SYSCALL:
			syscalls++

			expected, err = strconv.Atoi(data["items"])
			if err != nil {
				return fault.Wrap("Audit PATH count", err)
			}
		case auparse.AUDIT_PATH:
			if items[data["item"]] {
				return fault.New("duplicate Audit PATH item")
			}

			items[data["item"]] = true
		case auparse.AUDIT_PROCTITLE, auparse.AUDIT_EOE:
			finished = true
		}
	}

	if !finished || syscalls != 1 || expected < 1 || expected != len(items) {
		return fault.New("incomplete Audit syscall")
	}

	return nil
}

func recordedActor(value *aucoalesce.Event) (event.Actor, error) {
	pid, err := strconv.Atoi(value.Process.PID)
	if err != nil || pid <= 0 {
		return event.Actor{}, fault.New("invalid Audit PID")
	}

	uid, uidErr := strconv.ParseUint(value.User.IDs["uid"], 10, 32)

	euid, euidErr := strconv.ParseUint(value.User.IDs["euid"], 10, 32)
	if uidErr != nil || euidErr != nil {
		return event.Actor{}, fault.New("invalid Audit UID")
	}

	actor := event.Actor{
		PID: pid, Known: true, UserKnown: true,
		Executable: value.Process.Exe, Command: value.Process.Title,
		Evidence: fmt.Sprintf("audit:%d:%d", value.Timestamp.UnixMilli(), value.Sequence),
	}
	actor.UID, actor.EUID = uint32(uid), uint32(euid)

	login, loginErr := strconv.ParseUint(value.User.IDs["auid"], 10, 32)
	if loginErr == nil && login < math.MaxUint32 {
		actor.LoginUID, actor.LoginKnown = uint32(login), true
	}

	live := (procattr.Provider{}).ResolveAt(context.Background(), event.Raw{PID: pid}, value.Timestamp)
	if live.Known && live.UserKnown && live.UID == actor.UID && live.EUID == actor.EUID &&
		live.Executable == actor.Executable {
		actor.StartTime = live.StartTime
	}

	return actor, nil
}

func auditOperation(data map[string]string) string {
	switch data["syscall"] {
	case "write", "writev", "pwrite64", "pwritev", "pwritev2", "truncate", "ftruncate", "fallocate":
		return event.Write
	case "creat", "mkdir", "mkdirat", "mknod", "mknodat", "link", "linkat", "symlink", "symlinkat":
		return event.Create
	case "unlink", "unlinkat", "rmdir":
		return event.Remove
	case "rename", "renameat", "renameat2":
		return event.Rename
	case "chmod", "fchmod", "fchmodat", "fchmodat2", "chown", "lchown", "fchown", "fchownat",
		"utime", "utimes", "futimesat", "utimensat", "setxattr", "fsetxattr", "lsetxattr", "removexattr":
		return event.Attrib
	case "open":
		return writableOpen(data["a1"])
	case "openat":
		return writableOpen(data["a2"])
	default:
		return ""
	}
}

func writableOpen(value string) string {
	flags, err := strconv.ParseUint(value, 16, 64)
	if err != nil || flags&(unix.O_WRONLY|unix.O_RDWR|unix.O_CREAT|unix.O_TRUNC) == 0 {
		return ""
	}

	return event.Write
}

func validateTransaction(messages []*auparse.AuditMessage) error {
	if len(messages) == 0 || messages[0] == nil {
		return fault.New("empty Audit transaction")
	}

	first := messages[0]
	for _, message := range messages {
		if message == nil || message.Sequence != first.Sequence || !message.Timestamp.Equal(first.Timestamp) {
			return fault.New("mixed Audit transactions")
		}
	}

	return nil
}

// A historical dirfd cannot be resolved safely through the current /proc table.
// Ambiguous relative *at paths remain unknown, including paths already deleted.
func cwdRelative(data map[string]string) bool {
	switch data["syscall"] {
	case "renameat", "renameat2", "linkat":
		return auditCWD(data["a0"]) && auditCWD(data["a2"])
	case "symlinkat":
		return auditCWD(data["a1"])
	case "openat", "unlinkat", "mkdirat", "mknodat", "fchmodat", "fchmodat2", "fchownat", "futimesat", "utimensat":
		return auditCWD(data["a0"])
	default:
		return true
	}
}

func auditCWD(argument string) bool {
	// Linux may report a sign-extended 64-bit value or its 32-bit representation.
	return argument == "ffffff9c" || argument == "ffffffffffffff9c"
}

func recordedPath(name string, value *aucoalesce.Event) string {
	if !filepath.IsAbs(name) {
		if !filepath.IsAbs(value.Process.CWD) || !cwdRelative(value.Data) {
			return ""
		}

		name = filepath.Join(value.Process.CWD, name)
	}

	if strings.ContainsRune(name, 0) {
		return ""
	}

	return filepath.Clean(name)
}
