package cli

import (
	"flag"
	"io"
	"slices"
	"testing"
)

const (
	repo       = "repo"
	file       = "file"
	initAction = "init"
)

func TestSplitArgumentsKeepsFlagValuesAwayFromOperands(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name              string
		args              []string
		options, operands []string
	}{
		{"operands first", []string{"db", repo, "-c", file}, []string{"-c", file}, []string{"db", repo}},
		{"value between operands", []string{"db", "-c", file, repo}, []string{"-c", file}, []string{"db", repo}},
		{
			"a flag value is not an action",
			[]string{"--repository", initAction, "accept"},
			[]string{"--repository", initAction},
			[]string{"accept"},
		},
		{"inline value", []string{"--repository=init", "replace"}, []string{"--repository=init"}, []string{"replace"}},
		{"boolean flag takes nothing", []string{"--debug", initAction}, []string{"--debug"}, []string{initAction}},
		{
			"separator keeps dashed names",
			[]string{"-c", file, "--", "-db", repo},
			[]string{"-c", file},
			[]string{"-db", repo},
		},
		{
			"a lone dash is an operand and hides no later option",
			[]string{"-", "-c", file},
			[]string{"-c", file},
			[]string{"-"},
		},
		{"unknown flag is left to the parser", []string{"--bogus", initAction}, []string{"--bogus"}, []string{initAction}},
		{"missing value is left to the parser", []string{"db", repo, "-c"}, []string{"-c"}, []string{"db", repo}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			flags.String("c", "", "")
			flags.String("repository", "", "")
			flags.Bool("debug", false, "")

			options, operands := splitArguments(flags, testCase.args)
			if !slices.Equal(options, testCase.options) || !slices.Equal(operands, testCase.operands) {
				t.Fatalf("options %q operands %q", options, operands)
			}
		})
	}
}
