package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pointLink replaces the source parent link, so a test can make that source unreadable and back.
func pointLink(t *testing.T, link, target string) {
	t.Helper()

	err := os.Remove(link)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	err = os.Symlink(target, link)
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRefusesUnreadableLiteralSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	link := filepath.Join(root, "link")

	err := os.MkdirAll(filepath.Join(root, "real", "data"), 0o700)
	if err == nil {
		err = os.WriteFile(filepath.Join(root, "real", "data", "file"), []byte("observed"), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}

	pointLink(t, link, "real")

	invoke := inventoryRepository(t, root, "type: db\npaths: ['"+link+"/data']\n")

	verifiedChanges(t, invoke)

	// A symlink loop exceeds the hop limit, which leaves the source unread even for root.
	pointLink(t, link, "link")

	output, err := invoke(verifyCommand)
	if err == nil || !strings.Contains(err.Error(), "incomplete source selection") ||
		!strings.Contains(err.Error(), link+"/data") {
		t.Fatalf("unreadable source was reconciled as deleted: %v %s", err, output)
	}

	pointLink(t, link, "real")

	if changed := verifiedChanges(t, invoke); changed != 0 {
		t.Fatalf("readable source again reported %d changes", changed)
	}
}
