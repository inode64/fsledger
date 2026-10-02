package redact_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/redact"
)

func TestMaskCompleteMultilineText(t *testing.T) {
	t.Parallel()

	masker, err := redact.Compile([]redact.Rule{
		{Pattern: `(?m)^(TOKEN=).*$`, Replacement: `${1}[OCULTO]`},
		{Pattern: `(?s)-----BEGIN PRIVATE KEY-----.*?-----END PRIVATE KEY-----`, Replacement: `[CLAVE OCULTA]`},
	})
	if err != nil {
		t.Fatal(err)
	}

	input := "TOKEN=real-secret\nnormal=true\n-----BEGIN PRIVATE KEY-----\ninside-secret\n-----END PRIVATE KEY-----\n"

	output, err := masker.Apply(input)
	if err != nil || output != "TOKEN=[OCULTO]\nnormal=true\n[CLAVE OCULTA]\n" {
		t.Fatal(output, err)
	}

	if !strings.Contains(input, "real-secret") {
		t.Fatal("source mutated")
	}
}

func TestRedactionLimitsFailClosed(t *testing.T) {
	t.Parallel()

	for _, pattern := range []string{"", "[", ".*"} {
		_, err := redact.Compile([]redact.Rule{{Pattern: pattern, Replacement: "masked"}})
		if err == nil {
			t.Fatal("invalid rule accepted", pattern)
		}
	}

	masker, err := redact.Compile([]redact.Rule{{Pattern: "a", Replacement: strings.Repeat("b", 128)}})
	if err != nil {
		t.Fatal(err)
	}

	for _, input := range []string{
		strings.Repeat("a", 1025),
		strings.Repeat("a", 600),
		strings.Repeat("z", redact.MaxTextBytes+1),
	} {
		output, applyErr := masker.Apply(input)
		if applyErr == nil || output != "" {
			t.Fatal("unsafe partial redaction returned", applyErr)
		}
	}
}
