// Package cli implements shared foreground and daemon entry points.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/inode64/fsledger/internal/fault"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
	"github.com/inode64/fsledger/internal/doctor"
	"github.com/inode64/fsledger/internal/notify"
)

// Run parses commands and returns errors to the thin executable entry points.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fault.New(usageSummary)
	}

	command := args[0]
	if command == "--help" || command == "-h" || command == "help" {
		_, err := fmt.Fprintln(stdout, usageText)

		return fault.Wrap("write help", err)
	}

	if command == "--version" || command == "-version" || command == "version" {
		return printVersion(stdout)
	}

	if command == commandRestore {
		return runRestore(args[1:], stdout, stderr)
	}

	return runConfigured(command, args[1:], stdout, stderr)
}

func runConfigured(command string, arguments []string, stdout, stderr io.Writer) error {
	action := ""

	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("c", config.DefaultPath, "configuration path")

	debug := flags.Bool("debug", false, "debug logging")

	effective := flags.Bool("effective", false, "show resolved configuration without credentials")
	repository := flags.String("repository", "", "repository name for flush, verify, report preview and outbox clear")
	forget := flags.Bool("forget-unmounted", false,
		"verify: forget recorded filesystems that are not mounted and record what is there now")

	operands, done, err := parseCommand(flags, arguments, stdout)
	if done {
		return err
	}

	// An unknown command is reported as such, whatever operands follow it.
	if !slices.Contains(
		[]string{
			commandCheck,
			"doctor",
			"status",
			commandFlush,
			commandRun,
			commandReport,
			commandOutbox,
			commandVerify,
		},
		command,
	) {
		return fault.New(fmt.Sprintf("unknown command %q", command))
	}

	err = misplacedFlag(flags, command)
	if err != nil {
		return err
	}

	if (command == commandReport || command == commandOutbox) && len(operands) > 0 {
		action, operands = operands[0], operands[1:]
	}

	if len(operands) != 0 {
		return fault.New("unexpected arguments")
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	return dispatch(stdout, stderr, cfg, command, action, *repository, options{
		debug: *debug, effective: *effective, forgetUnmounted: *forget,
	})
}

// options are the switches a command may accept; misplacedFlag rejects the others.
type options struct {
	debug, effective, forgetUnmounted bool
}

// misplacedFlag rejects an option given to a command that would ignore it silently.
func misplacedFlag(flags *flag.FlagSet, command string) error {
	var err error

	flags.Visit(func(option *flag.Flag) {
		accepted := true

		switch option.Name {
		case "debug":
			accepted = command == commandRun
		case "effective":
			accepted = command == commandCheck
		case "repository":
			accepted = slices.Contains([]string{commandFlush, commandReport, commandOutbox, commandVerify}, command)
		case "forget-unmounted":
			accepted = command == commandVerify
		}

		if !accepted && err == nil {
			err = fault.New("--" + option.Name + " is not accepted by " + command)
		}
	})

	return err
}

func dispatch(
	stdout, stderr io.Writer,
	cfg *config.Config,
	command, action, repository string,
	switches options,
) error {
	var err error

	switch command {
	case commandCheck:
		err = checkConfiguration(cfg)
		if err != nil {
			return err
		}

		if switches.effective {
			return printEffective(stdout, cfg)
		}

		return printCheck(stdout, cfg)
	case "doctor":
		return doctor.Print(stdout, cfg)
	case "status":
		return printStatus(stdout, cfg)
	case commandFlush:
		err = daemon.RequestFlush(cfg, repository)
		if err != nil {
			return err
		}

		_, err = fmt.Fprintln(stdout, "Flush requested; check status for completion")

		return fault.Wrap("write flush result", err)
	case commandRun:
		return serve(stderr, cfg, switches.debug)
	case commandReport:
		if action != "preview" {
			return fault.New("report requires preview")
		}

		return previewReport(context.Background(), stdout, cfg, repository)
	case commandOutbox:
		if action != "clear" {
			return fault.New("outbox requires clear")
		}

		return clearOutbox(context.Background(), stdout, cfg, repository)
	case commandVerify:
		return verifyCommand(context.Background(), stdout, cfg, repository, switches.forgetUnmounted)
	default:
		return fault.New(fmt.Sprintf("unknown command %q", command))
	}
}

// checkConfiguration inspects state locations and renders notification templates, which loading only
// parses: one that cannot render an event would keep every alert to its notifier retrying.
func checkConfiguration(cfg *config.Config) error {
	err := cfg.CheckPaths()
	if err != nil {
		return err
	}

	return notify.CheckTemplates(cfg)
}

const (
	commandCheck   = "check"
	commandFlush   = "flush"
	commandRun     = "run"
	commandReport  = "report"
	commandRestore = "restore"
	commandOutbox  = "outbox"
	commandVerify  = "verify"
)
