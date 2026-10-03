package integrity

import (
	"strings"
	"testing"
)

func TestContentSizeRequiresExactReadableLength(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name, remainder  string
		expected, copied int64
		valid            bool
	}{
		{name: "empty regular file", expected: 0, copied: 0, remainder: "", valid: true},
		{name: "complete regular file", expected: 3, copied: 3, remainder: "", valid: true},
		{name: "virtual zero size", expected: 0, copied: 0, remainder: "content", valid: false},
		{name: "premature EOF", expected: 4, copied: 2, remainder: "", valid: false},
		{name: "extra content", expected: 3, copied: 3, remainder: "x", valid: false},
	} {
		err := CheckContentSize(strings.NewReader(fixture.remainder), fixture.expected, fixture.copied)
		if (err == nil) != fixture.valid {
			t.Fatal(fixture.name, err)
		}
	}
}

func TestHashRejectsVirtualFileWithZeroDeclaredSize(t *testing.T) {
	t.Parallel()

	_, err := NewScanner(1, 1).Observe(t.Context(), "/proc/version", "sha256", true)
	if err == nil || !strings.Contains(err.Error(), "source size differs") {
		t.Fatal("virtual file was accepted as empty", err)
	}
}
