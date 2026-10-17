package humanize_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/humanize"
)

func TestTime(t *testing.T) {
	t.Parallel()

	if got := humanize.Time(0); got != humanize.Absent {
		t.Fatalf("Time(0) = %q", got)
	}

	layout := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \S+$`)
	if got := humanize.Time(1792233661004600592); !layout.MatchString(got) {
		t.Fatalf("Time = %q", got)
	}

	precise := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.004600592 \S+$`)
	if got := humanize.PreciseTime(1792233661004600592); !precise.MatchString(got) {
		t.Fatalf("PreciseTime = %q", got)
	}
}

func TestSize(t *testing.T) {
	t.Parallel()

	for bytes, want := range map[int64]string{
		0:                  "0 B",
		1023:               "1023 B",
		1024:               "1.0 KiB",
		182422:             "178.1 KiB",
		1 << 20:            "1.0 MiB",
		5*(1<<30) + 1<<29:  "5.5 GiB",
		1<<62 + 1<<61:      "6.0 EiB",
		-2048:              "-2.0 KiB",
		1024*1024 - 1:      "1024.0 KiB",
		1024 * 1024 * 1024: "1.0 GiB",
	} {
		if got := humanize.Size(bytes); got != want {
			t.Errorf("Size(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestMode(t *testing.T) {
	t.Parallel()

	for mode, want := range map[uint32]string{
		0:        humanize.Absent,
		0o100644: "-rw-r--r-- (0644)",
		0o100440: "-r--r----- (0440)",
		0o100674: "-rw-rwxr-- (0674)",
		0o040755: "drwxr-xr-x (0755)",
		0o120777: "lrwxrwxrwx (0777)",
		0o104755: "-rwsr-xr-x (4755)",
		0o102644: "-rw-r-Sr-- (2644)",
		0o041777: "drwxrwxrwt (1777)",
		0o041776: "drwxrwxrwT (1776)",
		0o020620: "crw--w---- (0620)",
		0o060660: "brw-rw---- (0660)",
		0o010600: "prw------- (0600)",
		0o140755: "srwxr-xr-x (0755)",
	} {
		if got := humanize.Mode(mode); got != want {
			t.Errorf("Mode(%o) = %q, want %q", mode, got, want)
		}
	}
}

func TestPrintable(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"/usr/lib64/php7.4/bin/php-fpm": "/usr/lib64/php7.4/bin/php-fpm",
		"línea con eñe":                 "línea con eñe",
		"evil\nRuta: \"/etc/shadow\"":   `evil\nRuta: "/etc/shadow"`,
		"cr\rtab\tesc\x1b[31m":          `cr\rtab\tesc\x1b[31m`,
		"bad\xffbyte":                   `bad\xffbyte`,
		`back\slash`:                    `back\\slash`,
		"sep\u2028":                     `sep\u2028`,
	} {
		got := humanize.Printable(input)
		if got != want {
			t.Errorf("Printable(%q) = %q, want %q", input, got, want)
		}

		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("Printable(%q) kept a line break", input)
		}
	}
}
