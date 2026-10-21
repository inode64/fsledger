package cli_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	changeFlag     = "--change"
	inputFlag      = "--input"
	importAction   = "import"
	changeID       = "ID"
	initAction     = "init"
	acceptRequired = "--change requires baseline accept"
	changeRefused  = "baseline import requires --input; --change is not accepted"
)

func TestArgumentFlagsAreRefusedOutsideTheirAction(t *testing.T) {
	t.Parallel()

	root, invoke := writeInventoryFixture(t, "db")
	input := filepath.Join(root, "reviewed.jsonl")

	for name, scenario := range map[string]struct {
		want string
		args []string
	}{
		// A change ID must never be opened as the reviewed reference.
		"import with change only": {want: changeRefused, args: []string{baselineCommand, importAction, changeFlag, input}},
		"import with both": {
			want: changeRefused, args: []string{baselineCommand, importAction, changeFlag, changeID, inputFlag, input},
		},
		"import without input": {want: "baseline import requires --input", args: []string{baselineCommand, importAction}},
		"verify with change":   {want: acceptRequired, args: []string{verifyCommand, changeFlag, changeID}},
		"changes with change":  {want: acceptRequired, args: []string{changesCommand, changeFlag, changeID}},
		"init with change":     {want: acceptRequired, args: []string{baselineCommand, initAction, changeFlag, changeID}},
		"replace with change":  {want: acceptRequired, args: []string{baselineCommand, "replace", changeFlag, changeID}},
		"accept with input": {
			want: "--input requires baseline import",
			args: []string{baselineCommand, "accept", changeFlag, changeID, inputFlag, input},
		},
	} {
		_, err := invoke(scenario.args...)
		if err == nil || !strings.Contains(err.Error(), scenario.want) {
			t.Fatalf("%s: want %q, got %v", name, scenario.want, err)
		}
	}

	_, err := os.Lstat(filepath.Join(root, "state"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused flag created repository state: %v", err)
	}

	// --input alone still reaches the import, which reports the missing reviewed reference.
	_, err = invoke(baselineCommand, importAction, inputFlag, input)
	if err == nil || !strings.Contains(err.Error(), "open reviewed reference") {
		t.Fatalf("import ignored --input: %v", err)
	}
}
