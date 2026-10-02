package config

import "io"

// Parse applies defaults before decoding; explicit false values remain false.
func Parse(reader io.Reader) (*Config, error) {
	cfg := defaults()

	_, err := decodeDocument(reader, cfg, maxConfigBytes)
	if err != nil {
		return nil, err
	}

	err = cfg.validateLogging()
	if err != nil {
		return nil, err
	}

	err = cfg.validateGlobals()
	if err != nil {
		return nil, err
	}

	err = cfg.validateOptions()
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// ParseFileDocument decodes one configuration document without accessing disk.
func ParseFileDocument(reader io.Reader, target any) error {
	_, err := decodeDocument(reader, target, maxConfigBytes)

	return err
}

// ResolveSourcesTrusting resolves sources with a substituted symlink owner policy.
func (c *Config) ResolveSourcesTrusting(trusted func(uid uint32) bool) map[string]Sources {
	return c.resolveSources("", trusted)
}
