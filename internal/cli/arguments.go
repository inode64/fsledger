package cli

import (
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
)

const (
	// usageSummary fits the single log line in which the entry points report errors.
	usageSummary = "usage: fsledger check|doctor|status|run|flush|verify|changes|baseline|report|migrate_aide|version " +
		"[-c configuration]"
	usageText = "Usage: fsledger check|doctor|status|run|flush|verify|changes|baseline [-c configuration] [--debug]\n" +
		"       fsledger report preview --repository NAME [-c configuration]\n" +
		"       " + migrationUsage + "\n" +
		"       fsledger --version"
	migrationUsage = "fsledger migrate_aide <db_old> <repository> [-c configuration]"
)

// splitArguments lets options and operands appear in any order without mistaking a flag value for
// an operand; -- keeps dash-prefixed operands. Unknown flags are left for the parser to report.
func splitArguments(flags *flag.FlagSet, args []string) ([]string, []string) {
	var options, operands []string

	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return options, append(operands, args[index+1:]...)
		}

		// The flag package stops at a lone dash, which would silently drop every later option.
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			operands = append(operands, argument)

			continue
		}

		options = append(options, argument)
		name, _, inline := strings.Cut(strings.TrimLeft(argument, "-"), "=")

		option := flags.Lookup(name)
		if option == nil || inline || index+1 == len(args) {
			continue
		}

		boolean, ok := option.Value.(interface{ IsBoolFlag() bool })
		if !ok || !boolean.IsBoolFlag() {
			index++
			options = append(options, args[index])
		}
	}

	return options, operands
}

// parseCommand parses the options shared by every command and returns the operands. It reports
// done after serving --help or --version.
func parseCommand(flags *flag.FlagSet, args []string, stdout io.Writer) ([]string, bool, error) {
	showVersion := flags.Bool("version", false, "print version and exit")
	options, operands := splitArguments(flags, args)

	err := flags.Parse(options)
	if errors.Is(err, flag.ErrHelp) {
		return nil, true, nil
	}

	if err != nil {
		return nil, true, fault.Wrap("parse arguments", err)
	}

	if *showVersion {
		return nil, true, printVersion(stdout)
	}

	return operands, false, nil
}
