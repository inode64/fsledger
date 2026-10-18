package catalog

import (
	"os/user"

	"github.com/inode64/fsledger/internal/humanize"
)

// Resolve names on the sending host at render time. Numeric IDs remain the evidence;
// deleted accounts and failed lookups must not prevent a notification from being sent.
func displayUID(value string) string {
	account, err := user.LookupId(value)
	if err != nil || account.Username == "" {
		return value
	}

	return value + suffixOpening + humanize.Printable(account.Username) + suffixClosing
}

func displayGID(value string) string {
	group, err := user.LookupGroupId(value)
	if err != nil || group.Name == "" {
		return value
	}

	return value + suffixOpening + humanize.Printable(group.Name) + suffixClosing
}
