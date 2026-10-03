package aide

import "testing"

func TestEmptyAndAbsentAIDELinkTargetsRemainDistinct(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		encoded, target string
		valid           bool
	}{
		{encoded: "0-", target: "", valid: true},
		{encoded: "00-", target: "0-", valid: true},
		{encoded: "0", target: "", valid: false},
	} {
		entry, err := decode(
			[]string{"name", "perm", "lname"},
			[]string{"/link", "120777", fixture.encoded},
			"sha256",
			32,
		)
		if (err == nil) != fixture.valid {
			t.Fatal(fixture.encoded, err)
		}

		if fixture.valid && (entry.Record.Target == nil || string(entry.Record.Target) != fixture.target) {
			t.Fatal("incorrect link target", fixture.encoded, entry.Record.Target)
		}
	}
}
