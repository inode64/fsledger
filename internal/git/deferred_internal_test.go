package git

import (
	"strings"
	"testing"
)

func TestParseStagedVersionsPreservesRawPaths(t *testing.T) {
	t.Parallel()

	header := ":100644 100755 " + strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " M"
	path := "etc/line\nraw-\xff"

	versions, err := parseVersions(header + "\x00" + path + "\x00")
	if err != nil || versions["/"+path] != header || len(versions) != 1 {
		t.Fatal("lost index identity", versions, err)
	}

	invalidOutputs := []string{
		header,
		header + "\x00etc/path",
		header + "\x00../escape\x00",
		header + "\x00/etc/path\x00",
		header + "\x00etc/../path\x00",
		"bad\x00path\x00",
	}

	for _, invalid := range invalidOutputs {
		_, err = parseVersions(invalid)
		if err == nil {
			t.Fatal("accepted malformed index diff", invalid)
		}
	}
}
