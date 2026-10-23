package config

import (
	"crypto/sha256"
	"encoding/hex"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/humanize"
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

		var definition struct {
			Subject string `yaml:"subject"`
			Body    string `yaml:"body"`
		}

		err := loader.read(file, &definition)
		if err != nil {
			return fault.Wrap("invalid template document", err)
		}

		parsed, err := ParseTemplate(name, definition.Subject, definition.Body)
		if err != nil {
			return err
		}

		cfg.Templates[name] = parsed

		return nil
	})
}

// ParseTemplate validates and compiles a notification template once at configuration load.
func ParseTemplate(name, subject, body string) (Template, error) {
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(body) == "" {
		return Template{}, fault.New("template requires subject and body: " + name)
	}

	parsedSubject, err := NotificationTemplate(name + ":subject").Parse(subject)
	if err != nil {
		return Template{}, fault.New("invalid template subject: " + name)
	}

	parsedBody, err := NotificationTemplate(name + ":body").Parse(body)
	if err != nil {
		return Template{}, fault.New("invalid template body: " + name)
	}

	return Template{Subject: parsedSubject, Body: parsedBody}, nil
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

	if endpoint.Port() != "" {
		const maximumPort = 65535

		port, portErr := strconv.Atoi(endpoint.Port())
		if portErr != nil || port < 1 || port > maximumPort {
			return nil, fault.New("email port must be between 1 and 65535")
		}
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
// time keeps the documented UTC RFC 3339 output; localtime is the human rendering in the server zone.
func NotificationTemplate(name string) *template.Template {
	return template.New(name).Funcs(template.FuncMap{
		"time":      func(value int64) string { return time.Unix(0, value).UTC().Format(time.RFC3339Nano) },
		"localtime": humanize.Time,
		"duration":  humanize.Duration,
	}).Option("missingkey=error")
}
