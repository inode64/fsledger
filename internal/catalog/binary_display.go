package catalog

import (
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/inode64/fsledger/internal/humanize"
	"github.com/inode64/fsledger/internal/integrity"
)

// Symlink targets, xattrs and ACLs may hold any bytes, so they are stored as JSON base64. These
// views decode them for people; anything that does not decode is shown as stored.
const (
	attributeSeparator = ", "
	attributeAssign    = "="
	hexPrefix          = "0x"
	// cTerminator ends text values written by C programs, such as SELinux labels.
	cTerminator = "\x00"
)

// displayTarget shows a symlink target as text, escaping control characters and invalid UTF-8.
func displayTarget(value string) string {
	if value == "" {
		return humanize.Absent
	}

	var target []byte
	if json.Unmarshal([]byte(value), &target) != nil {
		return value
	}

	return humanize.Printable(string(target))
}

// displayAttributes lists attributes as getfattr does, name="text" or name=0x<hex>, keeping the
// readability status. Values are shown in full: two versions may differ anywhere.
func displayAttributes(value string) string {
	encoded, status := splitStatus(value)

	var attributes []integrity.Attribute
	if json.Unmarshal([]byte(encoded), &attributes) != nil {
		return value
	}

	entries := make([]string, len(attributes))
	for index, attribute := range attributes {
		entries[index] = humanize.Printable(string(attribute.Name)) + attributeAssign + attributeValue(attribute.Value)
	}

	shown := strings.Join(entries, attributeSeparator)
	if shown == "" {
		shown = humanize.Absent
	}

	if status == "" {
		return shown
	}

	return shown + suffixOpening + status + suffixClosing
}

// attributeValue quotes printable text; Quote escapes a C terminator and quotes, so a value cannot
// pose as another entry. Anything else is binary, such as an ACL or capability, and shown in hex.
func attributeValue(value []byte) string {
	text := strings.TrimSuffix(string(value), cTerminator)
	if utf8.ValidString(text) && !strings.ContainsFunc(text, unprintable) {
		return strconv.Quote(string(value))
	}

	return hexPrefix + hex.EncodeToString(value)
}

func unprintable(char rune) bool {
	return !unicode.IsPrint(char)
}
