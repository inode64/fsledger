// Package redact masks outbound AI text; it never changes archived content.
package redact

import (
	"regexp"

	"github.com/inode64/fsledger/internal/fault"
)

// Redaction limits bound memory before any content reaches an AI adapter.
const (
	MaxTextBytes   = 64 << 10
	maxMatches     = 1024
	maxReplacement = 128
	maxRules       = 64
	maxGroups      = 16
)

// Rule masks matching values with Go regexp capture expansion.
type Rule struct {
	Pattern     string `yaml:"pattern"`
	Replacement string `yaml:"replacement"`
}

type compiled struct {
	expression  *regexp.Regexp
	replacement string
}

// Masker applies validated rules to complete bounded texts.
type Masker struct{ rules []compiled }

// Compile validates bounded, nonempty matching rules.
func Compile(rules []Rule) (*Masker, error) {
	if len(rules) > maxRules {
		return nil, fault.New("too many redaction rules")
	}

	result := &Masker{}

	for _, rule := range rules {
		expression, err := regexp.Compile(rule.Pattern)
		if err != nil || len(rule.Replacement) > maxReplacement {
			return nil, fault.New("invalid AI redaction rule")
		}

		if expression.MatchString("") || expression.NumSubexp() > maxGroups {
			return nil, fault.New("AI redaction must consume text with at most 16 groups")
		}

		result.rules = append(result.rules, compiled{expression: expression, replacement: rule.Replacement})
	}

	return result, nil
}

// Apply masks the entire text before diff generation or context truncation.
func (masker *Masker) Apply(text string) (string, error) {
	if len(text) > MaxTextBytes {
		return "", fault.New("AI text exceeds redaction limit")
	}

	for _, rule := range masker.rules {
		replaced, err := rule.apply(text)
		if err != nil {
			return "", err
		}

		text = replaced
	}

	return text, nil
}

func (rule compiled) apply(text string) (string, error) {
	matches := rule.expression.FindAllStringSubmatchIndex(text, maxMatches+1)
	if len(matches) == 0 {
		return text, nil
	}

	if len(matches) > maxMatches {
		return "", fault.New("AI redaction match limit exceeded")
	}

	output := make([]byte, 0, len(text))

	offset := 0
	for _, match := range matches {
		output = append(output, text[offset:match[0]]...)

		output = rule.expression.ExpandString(output, rule.replacement, text, match)
		if len(output) > MaxTextBytes {
			return "", fault.New("AI redaction output limit exceeded")
		}

		offset = match[1]
	}

	output = append(output, text[offset:]...)
	if len(output) > MaxTextBytes {
		return "", fault.New("AI redaction output limit exceeded")
	}

	return string(output), nil
}
