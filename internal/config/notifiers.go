package config

import (
	"crypto/sha256"
	"encoding/hex"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/inode64/fsledger/internal/fault"
)

// MailSchemeSMTP and MailSchemeSMTPS keep DSN validation and transport selection aligned.
const (
	MailSchemeSMTP  = "smtp"
	MailSchemeSMTPS = "smtps"
)

func (loader *catalogLoader) templates(cfg *Config) error {
	return loader.catalog("", []string{cfg.Paths.Templates}, func(name, file string) error {
		if _, exists := cfg.Templates[name]; exists {
			return fault.New("duplicate template: " + name)
		}

		var definition Template

		err := loader.read(file, &definition)
		if err != nil {
			return fault.New("invalid template document: " + file)
		}

		if strings.TrimSpace(definition.Subject) == "" || strings.TrimSpace(definition.Body) == "" {
			return fault.New("template requires subject and body: " + name)
		}

		for _, candidate := range []struct{ part, source string }{
			{part: "subject", source: definition.Subject},
			{part: "body", source: definition.Body},
		} {
			_, err = NotificationTemplate(name + ":" + candidate.part).Parse(candidate.source)
			if err != nil {
				return fault.New("invalid template " + candidate.part + ": " + name)
			}
		}

		cfg.Templates[name] = definition

		return nil
	})
}

// MailURL validates the transport DSN without including credentials in errors.
func MailURL(dsn string) (*url.URL, error) {
	endpoint, err := url.Parse(dsn)
	if err != nil || endpoint.Hostname() == "" || endpoint.Path != "" || endpoint.Fragment != "" {
		return nil, fault.New("invalid email DSN")
	}

	if endpoint.Scheme != MailSchemeSMTP && endpoint.Scheme != MailSchemeSMTPS {
		return nil, fault.New("email DSN requires smtp or smtps")
	}

	query, err := url.ParseQuery(endpoint.RawQuery)
	if err != nil {
		return nil, fault.New("invalid email DSN parameters")
	}

	err = validateMailQuery(query, endpoint.Scheme)
	if err != nil {
		return nil, err
	}

	return endpoint, nil
}

func validateMailQuery(query url.Values, scheme string) error {
	for key, values := range query {
		if (key != "tls" && key != "tls_server_name") || len(values) != 1 {
			return fault.New("unknown or repeated email DSN parameter")
		}

		if key == "tls_server_name" && (values[0] == "" || strings.ContainsAny(values[0], "/:@ \t\r\n")) {
			return fault.New("invalid email TLS server name")
		}
	}

	if scheme == MailSchemeSMTP && query.Get("tls") != "starttls" {
		return fault.New("smtp requires tls=starttls")
	}

	if scheme == MailSchemeSMTPS && query.Has("tls") {
		return fault.New("smtps uses implicit TLS without a tls parameter")
	}

	return nil
}

func (c *Config) validateNotifiers() error {
	for name, notifier := range c.Notifiers {
		if !validObjectName(name) {
			return fault.New("invalid notifier name")
		}

		if notifier.Template != "" {
			if _, ok := c.Templates[notifier.Template]; !ok {
				return fault.New("unknown notification template for " + name)
			}
		}

		err := validateTransport(notifier)
		if err != nil {
			return fault.Wrap("notifier "+name, err)
		}
	}

	return nil
}

func validateTransport(notifier Notifier) error {
	switch notifier.Type {
	case NotifierEmail:
		return validateEmail(notifier)
	case NotifierSlack:
		return validateSlack(notifier)
	default:
		return fault.New("unknown notifier type")
	}
}

func validateEmail(notifier Notifier) error {
	_, err := MailURL(notifier.DSN)
	if err != nil {
		return err
	}

	if notifier.URL != "" || len(notifier.To) == 0 {
		return fault.New("email requires recipients and no webhook URL")
	}

	for _, address := range append(slices.Clone(notifier.To), notifier.From) {
		_, err := mail.ParseAddress(address)
		if err != nil {
			return fault.New("invalid email address")
		}
	}

	return nil
}

func validateSlack(notifier Notifier) error {
	endpoint, err := url.Parse(notifier.URL)
	if err != nil || endpoint.Scheme != httpsScheme || endpoint.Hostname() == "" || endpoint.User != nil ||
		endpoint.Fragment != "" {
		return fault.New("invalid HTTPS webhook")
	}

	if notifier.DSN != "" || notifier.From != "" || len(notifier.To) != 0 {
		return fault.New("Slack notifier contains email options")
	}

	return nil
}

// NotifierVersion detects destination replacement without persisting the secret itself.
func NotifierVersion(notifier Notifier) string {
	sum := sha256.Sum256(
		[]byte(
			strings.Join(
				[]string{notifier.Type, notifier.DSN, notifier.URL, notifier.From, strings.Join(notifier.To, "\x00")},
				"\x01",
			),
		),
	)

	return hex.EncodeToString(sum[:])
}

// NotificationTemplate shares validation and rendering functions across the config and sender adapters.
func NotificationTemplate(name string) *template.Template {
	return template.New(name).Funcs(template.FuncMap{
		"time": func(value int64) string { return time.Unix(0, value).UTC().Format(time.RFC3339Nano) },
	}).Option("missingkey=error")
}
