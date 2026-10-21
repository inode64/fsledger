package notify

import (
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
