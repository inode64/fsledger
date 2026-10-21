// Package cli implements shared foreground and daemon entry points.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

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

	if command == "migrate_aide" {
		return runAIDEMigration(args[1:], stdout, stderr)
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
	repository := flags.String("repository", "", "repository name for flush and inventory commands")
	input := flags.String("input", "", "reviewed baseline JSON stream for baseline import")
	change := flags.String("change", "", "recorded change ID to approve")

	operands, done, err := parseCommand(flags, arguments, stdout)
	if done {
		return err
	}

	if (command == commandBaseline || command == commandReport || command == commandOutbox) && len(operands) > 0 {
		action, operands = operands[0], operands[1:]
	}

	if len(operands) != 0 {
		return fault.New("unexpected arguments")
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	err = checkArgumentFlags(command, action, *change, *input)
	if err != nil {
		return err
	}

	return dispatch(stdout, stderr, cfg, command, action, *repository, *change, *input, *debug, *effective)
}

// checkArgumentFlags keeps --change and --input to the baseline action each selects: any other command
// would ignore them silently.
func checkArgumentFlags(command, action, change, input string) error {
	if command == commandBaseline && action == actionImport && change != "" {
		return fault.New("baseline import requires --input; --change is not accepted")
	}

	if change != "" && (command != commandBaseline || action != actionAccept) {
		return fault.New("--change requires baseline accept")
	}

	if input != "" && (command != commandBaseline || action != actionImport) {
		return fault.New("--input requires baseline import")
	}

	return nil
}

func dispatch(
	stdout, stderr io.Writer,
	cfg *config.Config,
	command, action, repository, change, input string,
	debug, effective bool,
) error {
	var err error

	switch command {
	case "check":
		err = checkConfiguration(cfg)
		if err != nil {
			return err
		}

		if effective {
			return printEffective(stdout, cfg)
		}

		return printCheck(stdout, cfg)
	case "doctor":
		return doctor.Print(stdout, cfg)
	case "status":
		return printStatus(stdout, cfg)
	case "flush":
		err = daemon.RequestFlush(cfg, repository)
		if err != nil {
			return err
		}

		_, err = fmt.Fprintln(stdout, "Flush requested; check status for completion")

		return fault.Wrap("write flush result", err)
	case "run":
		return serve(stderr, cfg, debug)
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
	case commandBaseline, commandVerify, commandChanges:
		return inventoryCommand(context.Background(), stdout, cfg, command, action, repository, change, input)
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
	commandReport   = "report"
	commandRestore  = "restore"
	commandOutbox   = "outbox"
	commandBaseline = "baseline"
	actionImport    = "import"
	commandVerify   = "verify"
	commandChanges  = "changes"
)
