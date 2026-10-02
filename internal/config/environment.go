package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/fault"
)

var hostVariable = regexp.MustCompile(`\$\{HOST\}|\$HOST\b`)

// Expand parsed strings, so environment contents cannot introduce YAML fields.
func expandHost(data []byte) ([]byte, error) {
	if !hostVariable.Match(data) {
		return data, nil
	}

	host, present := os.LookupEnv("HOST")
	if !present {
		var err error

		host, err = os.Hostname()
		if err != nil {
			return nil, fault.Wrap("resolve HOST", err)
		}
	}

	if host == "" {
		return nil, fault.New("HOST must not be empty")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))

	var document yaml.Node

	err := decoder.Decode(&document)
	if err != nil {
		return nil, fault.Wrap("decode HOST document", err)
	}

	var extra any

	err = decoder.Decode(&extra)
	if !errors.Is(err, io.EOF) {
		return nil, fault.New("configuration must contain one YAML document")
	}

	expandHostNode(&document, host)

	result, err := yaml.Marshal(&document)
	if err != nil {
		return nil, fault.Wrap("expand HOST document", err)
	}

	// decodeDocument bounds the expanded size against the remaining budget.
	return result, nil
}

func expandHostNode(node *yaml.Node, host string) {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		node.Value = hostVariable.ReplaceAllStringFunc(node.Value, func(string) string { return host })
	}

	for _, child := range node.Content {
		expandHostNode(child, host)
	}
}
