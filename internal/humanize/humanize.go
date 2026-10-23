// Package humanize formats stored evidence for people. It never changes the evidence itself:
// callers keep the raw values and use these helpers only when rendering messages.
package humanize

import (
	"fmt"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Absent marks a value that does not exist, such as the previous state of a created file.
const Absent = "—"

const (
	timeLayout    = "2006-01-02 15:04:05 MST"
	sizeUnit      = 1024
	day           = 24 * time.Hour
	daysThreshold = 2 * day
)

// Time renders Unix nanoseconds in the local time zone with second precision. Zero means absent.
func Time(nanoseconds int64) string {
	if nanoseconds == 0 {
		return Absent
	}

	//nolint:gosmopolitan // Alerts are read by the server's operators, in the server's time zone.
	return time.Unix(0, nanoseconds).Local().Format(timeLayout)
}

// Size renders a byte count with IEC units and one decimal above one KiB.
func Size(bytes int64) string {
	if bytes < sizeUnit && bytes > -sizeUnit {
		return strconv.FormatInt(bytes, 10) + " B"
	}

	value := float64(bytes)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	unit := 0

	for value /= sizeUnit; (value >= sizeUnit || value <= -sizeUnit) && unit < len(units)-1; value /= sizeUnit {
		unit++
	}

	return strconv.FormatFloat(value, 'f', 1, 64) + " " + units[unit]
}

// Positions of the execute letters in "-rwxrwxrwx".
const (
	ownerExecIndex = 3
	groupExecIndex = 6
	otherExecIndex = 9
	symbolicLength = 10
	classBits      = 3
	ownerShift     = 2 * classBits
)

// Duration renders a span in its largest whole unit: days from two days on, then hours, then minutes.
func Duration(span time.Duration) string {
	switch {
	case span >= daysThreshold:
		return strconv.FormatInt(int64(span/day), 10) + " d"
	case span >= time.Hour:
		return strconv.FormatInt(int64(span/time.Hour), 10) + " h"
	default:
		return strconv.FormatInt(int64(span/time.Minute), 10) + " min"
	}
}

// Mode renders a raw st_mode as ls-style permissions followed by the whole st_mode in hexadecimal,
// as stat -c %f prints it. Zero means absent.
func Mode(mode uint32) string {
	if mode == 0 {
		return Absent
	}

	const rwx = "rwx"

	text := make([]byte, 0, symbolicLength)
	text = append(text, typeLetter(mode))

	for shift := ownerShift; shift >= 0; shift -= classBits {
		for bit := range classBits {
			if mode&(1<<(shift+classBits-1-bit)) != 0 {
				text = append(text, rwx[bit])
			} else {
				text = append(text, '-')
			}
		}
	}

	special(text, ownerExecIndex, mode&unix.S_ISUID != 0, mode&unix.S_IXUSR != 0, 's')
	special(text, groupExecIndex, mode&unix.S_ISGID != 0, mode&unix.S_IXGRP != 0, 's')
	special(text, otherExecIndex, mode&unix.S_ISVTX != 0, mode&unix.S_IXOTH != 0, 't')

	return fmt.Sprintf("%s (0x%x)", text, mode)
}

func typeLetter(mode uint32) byte {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return '-'
	case unix.S_IFDIR:
		return 'd'
	case unix.S_IFLNK:
		return 'l'
	case unix.S_IFCHR:
		return 'c'
	case unix.S_IFBLK:
		return 'b'
	case unix.S_IFIFO:
		return 'p'
	case unix.S_IFSOCK:
		return 's'
	default:
		return '?'
	}
}

// special overlays setuid, setgid or sticky on the execute position, as ls does.
func special(text []byte, index int, set, executable bool, letter byte) {
	if !set {
		return
	}

	if executable {
		text[index] = letter
	} else {
		text[index] = letter - 'a' + 'A'
	}
}

// Printable escapes control characters, invalid UTF-8 and backslashes with Go string-literal
// notation, so process-controlled text cannot add lines to a message or hide characters.
func Printable(text string) string {
	if isPlain(text) {
		return text
	}

	escaped := make([]byte, 0, len(text)+len(text)/4)

	for index := 0; index < len(text); {
		char, size := utf8.DecodeRuneInString(text[index:])

		switch {
		case char == utf8.RuneError && size == 1:
			escaped = fmt.Appendf(escaped, `\x%02x`, text[index])
		case char == '\\':
			escaped = append(escaped, `\\`...)
		case !unicode.IsPrint(char):
			quoted := strconv.QuoteRune(char)
			escaped = append(escaped, quoted[1:len(quoted)-1]...)
		default:
			escaped = utf8.AppendRune(escaped, char)
		}

		index += size
	}

	return string(escaped)
}

func isPlain(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}

	for _, char := range text {
		if char == '\\' || !unicode.IsPrint(char) {
			return false
		}
	}

	return true
}
