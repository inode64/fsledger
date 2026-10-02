// Package filter matches observed identities and paths without interpreting commit text.
package filter

import (
	"context"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
)

// Rule combines fields with AND; values and separate rules use OR.
type Rule struct {
	Users        []string `json:"users,omitempty"         yaml:"users"`
	CommandRegex []string `json:"command_regex,omitempty" yaml:"command_regex"`
	Paths        []string `json:"paths,omitempty"         yaml:"paths"`
}

type compiled struct {
	paths       *exclude.Matcher
	users       map[uint32]bool
	commands    []*regexp.Regexp
	requireUser bool
}

// Matcher is immutable after compilation and safe to share across workers.
type Matcher struct{ rules []compiled }

// Compile validates syntax and resolves account names on the host running fsledger.
// Absent named accounts do not match; heterogeneous hosts may share one policy.
func Compile(rules []Rule) (*Matcher, error) {
	result := &Matcher{rules: make([]compiled, 0, len(rules))}
	for _, rule := range rules {
		entry, err := compileRule(rule)
		if err != nil {
			return nil, err
		}

		result.rules = append(result.rules, entry)
	}

	return result, nil
}

func compileRule(rule Rule) (compiled, error) {
	result := compiled{paths: nil, users: nil, commands: nil, requireUser: len(rule.Users) > 0}
	if len(rule.Users)+len(rule.CommandRegex)+len(rule.Paths) == 0 ||
		(rule.Users != nil && len(rule.Users) == 0) || (rule.CommandRegex != nil && len(rule.CommandRegex) == 0) ||
		(rule.Paths != nil && len(rule.Paths) == 0) {
		return result, fault.New("filter rule must not be empty")
	}

	users, err := compileUsers(rule.Users)
	if err != nil {
		return result, err
	}

	result.users = users

	for _, pattern := range rule.CommandRegex {
		if pattern == "" {
			return result, fault.New("command_regex must not be empty")
		}

		expression, err := regexp.Compile(pattern)
		if err != nil {
			return result, fault.Wrap("invalid command_regex", err)
		}

		result.commands = append(result.commands, expression)
	}

	if len(rule.Paths) > 0 {
		matcher, err := exclude.New(rule.Paths...)
		if err != nil {
			return result, err
		}

		result.paths = matcher
	}

	return result, nil
}

// Match treats missing actor evidence as a nonmatch for identity-dependent fields.
func (matcher *Matcher) Match(path string, actor event.Actor) bool {
	for _, rule := range matcher.rules {
		if rule.matches(path, actor) {
			return true
		}
	}

	return false
}

func (rule compiled) matches(path string, actor event.Actor) bool {
	if rule.paths != nil && !rule.paths.Match(path) {
		return false
	}

	if rule.requireUser && (!actor.Known || !actor.UserKnown || !rule.users[actor.UID]) {
		return false
	}

	if len(rule.commands) == 0 {
		return true
	}

	if !actor.Known || actor.Command == "" {
		return false
	}

	for _, expression := range rule.commands {
		if expression.MatchString(actor.Command) {
			return true
		}
	}

	return false
}

type actorKey struct{}

// WithActors carries structured, per-path evidence for one serialized observation.
func WithActors(ctx context.Context, resolve func(string) event.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, resolve)
}

// Actor returns unknown outside an attributed event operation.
func Actor(ctx context.Context, path string) event.Actor {
	resolve, ok := ctx.Value(actorKey{}).(func(string) event.Actor)
	if !ok {
		return event.Actor{}
	}

	return resolve(path)
}

func compileUsers(names []string) (map[uint32]bool, error) {
	users := make(map[uint32]bool, len(names))
	for _, name := range names {
		identifier, numeric := strings.CutPrefix(name, "uid:")
		if name == "" {
			return nil, fault.New("filter user must not be empty")
		}

		if !numeric {
			account, err := user.Lookup(name)
			if err != nil {
				continue
			}

			identifier = account.Uid
		}

		uid, err := strconv.ParseUint(identifier, 10, 32)
		if err != nil {
			return nil, fault.Wrap("invalid filter UID", err)
		}

		users[uint32(uid)] = true
	}

	return users, nil
}
