package config_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

func TestHostExpansionIgnoresComments(t *testing.T) {
	t.Setenv("HOST", "")

	var value map[string]string

	err := config.ParseFileDocument(strings.NewReader("# served by $HOST\nname: fixed # ${HOST}\n"), &value)
	if err != nil || len(value) != 1 || value["name"] != "fixed" {
		t.Fatal("comment requested HOST expansion", value, err)
	}

	err = config.ParseFileDocument(strings.NewReader("# served by $HOST\nname: '${HOST}'\n"), &value)
	if err == nil {
		t.Fatal("empty HOST accepted for a requested expansion")
	}
}
