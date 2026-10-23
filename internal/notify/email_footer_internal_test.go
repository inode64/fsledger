package notify

import (
	"os"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/buildinfo"
)

func TestWithFooterSeparatesSignature(t *testing.T) {
	t.Parallel()

	signature := "\n-- \nFSledger | " + buildinfo.Release() + "\n" + emailSlogan + "\n" + projectURL + "\n"

	for _, body := range []string{"Body", "Body\n", "Body\n\n\n"} {
		if footed := withFooter(body); footed != "Body\n"+signature {
			t.Fatalf("withFooter(%q) = %q", body, footed)
		}
	}
}

func TestEmailSloganMatchesReadme(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(readme), "\n**"+emailSlogan+"**\n") {
		t.Fatal("the email footer slogan differs from the README tagline")
	}
}
