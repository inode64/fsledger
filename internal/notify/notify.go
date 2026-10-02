// Package notify delivers durable repository events through email and Slack adapters.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode/utf8"

	mail "github.com/wneessen/go-mail"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
)

const (
	slackMessageInterval = time.Second
	transportTimeout     = 15 * time.Second
	responseLimit        = 4096
	maxRetrySeconds      = 86400
	maxBackoffExponent   = 12
	digestMaxPaths       = 2000
	digestEmailBytes     = 512 << 10
	slackPartSize        = 12000
	digestSlackRunes     = 4 * slackPartSize
	digestMinDeliveries  = 2
	binarySearchDivisor  = 2
	defaultSubject       = `fsledger {{.Host}} / {{.Repository}}: {{.Event}} ` +
		`({{.Count}}: +{{.Added}} ~{{.Modified}} -{{.Deleted}})`
	defaultBody = `Servidor: {{.Host}} ({{.Hostname}})
Observaciones: {{.Count}} (+{{.Added}} ~{{.Modified}} -{{.Deleted}})
{{if .ChangeID}}Cambio: {{.ChangeID}}
{{end}}{{if .Reason}}Motivo: {{.Reason}}
{{end}}
Elementos cambiados
Clave  Ruta
{{range .Details}}{{printf "%-5s" .Code}}  {{printf "%q" .Path}}
{{else}}{{range .Paths}}—      {{printf "%q" .}}
{{end}}{{end}}
C = Creado; D = Borrado; M = Contenido; P = Solo permisos; MP = Contenido y permisos.
— = Otros metadatos o sin evidencia suficiente para clasificar.

Detalle de cambios
{{range .Details}}
Ruta: {{printf "%q" .Path}} / {{.Code}}
{{if .ChangeID}}ID de cambio: {{.ChangeID}}
{{end}}Observado: {{time .Observed}}
Actor: {{printf "%q" .Actor}}
{{range .VisibleFields}}{{.Field}}: {{.Before}} -> {{.After}}
{{else}}Sin diferencias de campos visibles.
{{end}}{{with .VisibleBaseline}}Comparación con referencia aprobada:
{{range .}}{{.Field}}: {{.Before}} -> {{.After}}
{{end}}{{end}}{{end}}`
)

var errLocalRateLimit = errors.New("slack delivery rate limited")

type compiledTemplate struct {
	subject *template.Template
	body    *template.Template
}

type renderedNotification struct {
	subject   string
	body      string
	messageID string
}

// Sender contains injectable clients for isolated HTTP/SMTP transport tests.
type Sender struct {
	slackEscaper *strings.Replacer
	next         map[string]time.Time
	HTTP         *http.Client
	TLS          *tls.Config
	templates    map[string]compiledTemplate
	mutex        sync.Mutex
}

// New constructs bounded transport clients; redirects cannot leak a webhook secret.
func New(templates map[string]config.Template) *Sender {
	compiled := make(map[string]compiledTemplate, len(templates)+1)

	for name, definition := range templates {
		subject, subjectErr := config.NotificationTemplate(name + ":subject").Parse(definition.Subject)

		body, bodyErr := config.NotificationTemplate(name + ":body").Parse(definition.Body)
		if subjectErr == nil && bodyErr == nil {
			compiled[name] = compiledTemplate{subject: subject, body: body}
		}
	}

	compiledSubject := template.Must(config.NotificationTemplate("notification:subject").Parse(defaultSubject))
	compiledBody := template.Must(config.NotificationTemplate("notification:body").Parse(defaultBody))
	compiled[""] = compiledTemplate{subject: compiledSubject, body: compiledBody}

	return &Sender{
		slackEscaper: strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;"),
		mutex:        sync.Mutex{}, next: make(map[string]time.Time),
		HTTP: &http.Client{
			Transport: nil, Jar: nil,
			Timeout:       transportTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
		TLS:       nil,
		templates: compiled,
	}
}

type limitedBuffer struct {
	bytes.Buffer

	remaining int
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > buffer.remaining {
		return 0, fault.New("notification template exceeds output limit")
	}

	count, err := buffer.Buffer.Write(data)
	buffer.remaining -= count

	return count, fault.Wrap("render notification", err)
}

// Send returns a retry delay and sanitized failure; it never logs the destination secret.
func (sender *Sender) Send(
	ctx context.Context,
	notifier config.Notifier,
	message catalog.Message,
) (time.Duration, error) {
	if notifier.Type == config.NotifierSlack {
		sender.mutex.Lock()
		delay := time.Until(sender.next[notifier.URL])
		sender.mutex.Unlock()

		if delay > 0 {
			return delay, errLocalRateLimit
		}
	}

	rendered, err := sender.render(notifier, message)
	if err != nil {
		return time.Hour, err
	}

	return sender.deliver(ctx, notifier, rendered)
}

// Run retries the persistent outbox independently of repository work.
func (sender *Sender) Run(
	ctx context.Context,
	store *catalog.Store,
	notifiers map[string]config.Notifier,
	logger *slog.Logger,
) {
	routing := destinations(notifiers)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := sender.deliverPending(ctx, store, routing, logger)
			if err != nil && ctx.Err() == nil {
				logger.Warn("outbox update failed", "error", err)
			}
		}
	}
}

// deliver sends text rendered once, whether for a single message or while sizing a digest.
func (sender *Sender) deliver(
	ctx context.Context,
	notifier config.Notifier,
	rendered renderedNotification,
) (time.Duration, error) {
	switch notifier.Type {
	case config.NotifierSlack:
		delay, err := sender.reserve(ctx, notifier.URL)
		if err != nil {
			return delay, err
		}

		delay, sendErr := sender.slackParts(ctx, notifier.URL, rendered.subject+"\n"+rendered.body)
		if delay > 0 {
			sender.mutex.Lock()
			sender.next[notifier.URL] = time.Now().Add(delay)
			sender.mutex.Unlock()
		}

		return delay, sendErr
	case config.NotifierEmail:
		return 0, sender.email(ctx, notifier, rendered)
	default:
		return time.Hour, fault.New("notifier unavailable")
	}
}

func finishDeliveries(
	ctx context.Context,
	store *catalog.Store,
	deliveries []catalog.Delivery,
	delay time.Duration,
	sendErr error,
) error {
	identifiers := make([]int64, len(deliveries))
	attempts := 0

	for index, delivery := range deliveries {
		identifiers[index] = delivery.ID
		attempts = max(attempts, delivery.Attempts)
	}

	switch {
	case sendErr == nil:
		return store.DeliveredMany(ctx, identifiers)
	case errors.Is(sendErr, errLocalRateLimit):
		return store.DeferMany(ctx, identifiers, delay)
	}

	backoff := time.Second * time.Duration(1<<min(attempts+1, maxBackoffExponent))

	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(time.Second)))
	if err == nil {
		backoff += time.Duration(jitter.Int64())
	}

	return store.RetryMany(ctx, identifiers, max(delay, backoff), sendErr.Error())
}

type destination struct {
	version  string
	notifier config.Notifier
}

func destinations(notifiers map[string]config.Notifier) map[string]destination {
	result := make(map[string]destination, len(notifiers))
	for name, notifier := range notifiers {
		result[name] = destination{notifier: notifier, version: config.NotifierVersion(notifier)}
	}

	return result
}

func (sender *Sender) render(notifier config.Notifier, message catalog.Message) (renderedNotification, error) {
	if message.Event == catalog.ReportEvent {
		return renderReport(message)
	}

	parsed, exists := sender.templates[notifier.Template]
	if !exists {
		return renderedNotification{}, fault.New("notification template unavailable")
	}

	const maxMessage = 16 << 20

	subject := limitedBuffer{Buffer: bytes.Buffer{}, remaining: maxMessage}

	err := parsed.subject.Execute(&subject, message)
	if err != nil {
		return renderedNotification{}, fault.New("notification subject execution failed")
	}

	if strings.ContainsAny(subject.String(), "\r\n") {
		return renderedNotification{}, fault.New("notification subject contains a line break")
	}

	body := limitedBuffer{Buffer: bytes.Buffer{}, remaining: subject.remaining}

	err = parsed.body.Execute(&body, message)
	if err != nil {
		return renderedNotification{}, fault.New("notification body execution failed")
	}

	return renderedNotification{subject: subject.String(), body: body.String(), messageID: ""}, nil
}

func (sender *Sender) slack(ctx context.Context, endpoint, body string) (time.Duration, error) {
	data, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: sender.slackEscaper.Replace(body)})
	if err != nil {
		return 0, fault.New("cannot encode Slack message")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, fault.New("invalid Slack request")
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := sender.HTTP.Do(request)
	if err != nil {
		return 0, fault.New("Slack transport failed")
	}
	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			slog.Warn("close notification response", "error", closeErr)
		}
	}()

	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
	if err != nil {
		return 0, fault.New("Slack response read failed")
	}

	if response.StatusCode == http.StatusTooManyRequests {
		seconds, parseErr := strconv.Atoi(response.Header.Get("Retry-After"))
		if parseErr != nil || seconds < 1 {
			seconds = 60
		}

		return time.Duration(min(seconds, maxRetrySeconds)) * time.Second, fault.New("Slack rate limit")
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return time.Minute, fault.New(fmt.Sprintf("slack HTTP status %d", response.StatusCode))
	}

	return 0, nil
}

func (sender *Sender) email(ctx context.Context, notifier config.Notifier, rendered renderedNotification) error {
	client, err := sender.emailClient(notifier)
	if err != nil {
		return err
	}

	message := mail.NewMsg()

	err = message.From(notifier.From)
	if err != nil {
		return fault.New("invalid email sender")
	}

	err = message.To(notifier.To...)
	if err != nil {
		return fault.New("invalid email recipients")
	}

	message.Subject(rendered.subject)
	message.SetBodyString(mail.TypeTextPlain, rendered.body)

	if rendered.messageID != "" {
		message.SetMessageIDWithValue(rendered.messageID)
	}

	err = client.DialAndSendWithContext(ctx, message)

	return emailDeliveryError(err)
}

func (sender *Sender) deliverPending(
	ctx context.Context,
	store *catalog.Store,
	notifiers map[string]destination,
	logger *slog.Logger,
) error {
	deliveries, err := store.Due(ctx)
	if err != nil {
		return err
	}

	for _, digest := range catalog.MergeMessages(deliveries) {
		err = ctx.Err()
		if err != nil {
			return fault.Wrap("delivery cancelled", err)
		}

		err = sender.deliverDigest(ctx, store, notifiers, logger, digest)
		if err != nil {
			return err
		}
	}

	return nil
}

func (sender *Sender) deliverDigest(
	ctx context.Context,
	store *catalog.Store,
	notifiers map[string]destination,
	logger *slog.Logger,
	digest catalog.Digest,
) error {
	target, ok := notifiers[digest.Destination]
	delay := time.Hour

	var sendErr error

	if ok && (digest.Version == "" || digest.Version == target.version) {
		var rendered *renderedNotification

		digest, rendered = sender.digestChunk(target.notifier, digest)
		if rendered != nil {
			delay, sendErr = sender.deliver(ctx, target.notifier, *rendered)
		} else {
			delay, sendErr = sender.Send(ctx, target.notifier, digest.Message)
		}
	} else {
		sendErr = fault.New("destination removed or changed")
	}

	if sendErr != nil && !errors.Is(sendErr, errLocalRateLimit) {
		logger.Warn("notification delivery deferred", "destination", digest.Destination, "error", sendErr)
	}

	err := finishDeliveries(ctx, store, digest.Deliveries, delay, sendErr)
	if err != nil {
		return err
	}

	if sendErr == nil {
		logDigest(logger, digest)
	}

	return nil
}

func logDigest(logger *slog.Logger, digest catalog.Digest) {
	logger.Info(
		"notification digest delivered",
		"repository", digest.Message.Repository,
		"event", digest.Message.Event,
		"destination", digest.Destination,
		"deliveries", len(digest.Deliveries),
		"paths", digest.Message.Count,
		"added", digest.Message.Added(),
		"modified", digest.Message.Modified(),
		"deleted", digest.Message.Deleted(),
		"first_path", firstPath(digest.Message),
	)
}

// digestChunk returns the longest prefix that fits the destination, with the text rendered while
// measuring it so the accepted candidate is neither merged nor rendered again.
func (sender *Sender) digestChunk(
	notifier config.Notifier,
	digest catalog.Digest,
) (catalog.Digest, *renderedNotification) {
	if len(digest.Deliveries) < digestMinDeliveries {
		return digest, nil
	}

	upper := len(digest.Deliveries)
	paths := 0

	for index, delivery := range digest.Deliveries {
		if index > 0 && paths+delivery.Message.Count > digestMaxPaths {
			upper = index

			break
		}

		paths += delivery.Message.Count
	}

	candidate := mergedPrefix(digest, upper)
	if upper == 1 {
		// A single delivery is indivisible even when oversized; Send renders it once.
		return candidate, nil
	}

	if rendered, fits := sender.digestFits(notifier, candidate.Message); fits {
		return candidate, &rendered
	}

	best := mergedPrefix(digest, 1)

	var bestRendered *renderedNotification

	low, high := 2, upper-1
	for low <= high {
		middle := low + (high-low)/binarySearchDivisor
		candidate = mergedPrefix(digest, middle)

		if rendered, fits := sender.digestFits(notifier, candidate.Message); fits {
			best, bestRendered = candidate, &rendered
			low = middle + 1
		} else {
			high = middle - 1
		}
	}

	return best, bestRendered
}

func mergedPrefix(digest catalog.Digest, count int) catalog.Digest {
	if count == len(digest.Deliveries) {
		return digest
	}

	return catalog.MergeMessages(digest.Deliveries[:count])[0]
}

func (sender *Sender) digestFits(
	notifier config.Notifier,
	message catalog.Message,
) (renderedNotification, bool) {
	rendered, err := sender.render(notifier, message)
	if err != nil {
		return rendered, false
	}

	text := rendered.subject + "\n" + rendered.body
	if notifier.Type == config.NotifierSlack {
		return rendered, utf8.RuneCountInString(text) <= digestSlackRunes
	}

	return rendered, len(text) <= digestEmailBytes
}

func firstPath(message catalog.Message) string {
	// Paths always lists what Details describe, so it alone says whether the message names any path.
	if len(message.Paths) > 0 {
		return message.Paths[0]
	}

	return ""
}

func (sender *Sender) reserve(ctx context.Context, key string) (time.Duration, error) {
	err := ctx.Err()
	if err != nil {
		return 0, fault.Wrap("notification cancelled", err)
	}

	sender.mutex.Lock()
	defer sender.mutex.Unlock()

	delay := time.Until(sender.next[key])
	if delay > 0 {
		return delay, errLocalRateLimit
	}

	sender.next[key] = time.Now().Add(slackMessageInterval)

	return 0, nil
}

// Slack messages are split without truncating the sole notification evidence.
func (sender *Sender) slackParts(ctx context.Context, endpoint, body string) (time.Duration, error) {
	characters := []rune(body)
	for offset := 0; offset < len(characters); offset += slackPartSize {
		if offset > 0 {
			timer := time.NewTimer(slackMessageInterval)
			select {
			case <-ctx.Done():
				timer.Stop()

				return 0, fault.Wrap("notification cancelled", ctx.Err())
			case <-timer.C:
			}
		}

		end := min(offset+slackPartSize, len(characters))

		delay, err := sender.slack(ctx, endpoint, string(characters[offset:end]))
		if err != nil {
			return delay, err
		}
	}

	return 0, nil
}

func defaultMailPort(scheme string) int {
	const submissionPort, implicitTLSPort = 587, 465
	if scheme == config.MailSchemeSMTPS {
		return implicitTLSPort
	}

	return submissionPort
}

func (sender *Sender) emailClient(notifier config.Notifier) (*mail.Client, error) {
	endpoint, err := config.MailURL(notifier.DSN)
	if err != nil {
		return nil, err
	}

	options := []mail.Option{
		mail.WithTimeout(transportTimeout),
		mail.WithTLSPolicy(mail.TLSMandatory),
		mail.WithPort(defaultMailPort(endpoint.Scheme)),
	}
	if endpoint.Scheme == config.MailSchemeSMTPS {
		options = append(options, mail.WithSSL())
	}

	if endpoint.Port() != "" {
		port, parseErr := strconv.Atoi(endpoint.Port())
		if parseErr != nil {
			return nil, fault.New("invalid email port")
		}

		options = append(options, mail.WithPort(port))
	}

	if tlsConfig := sender.mailTLS(endpoint.Query().Get("tls_server_name")); tlsConfig != nil {
		options = append(options, mail.WithTLSConfig(tlsConfig))
	}

	if endpoint.User != nil {
		password, _ := endpoint.User.Password()
		options = append(
			options,
			mail.WithSMTPAuth(mail.SMTPAuthPlain),
			mail.WithUsername(endpoint.User.Username()),
			mail.WithPassword(password),
		)
	}

	client, err := mail.NewClient(endpoint.Hostname(), options...)
	if err != nil {
		return nil, fault.New("invalid email transport options")
	}

	return client, nil
}

// mailTLS separates the dial address from the verified certificate identity for VPN relays.
func (sender *Sender) mailTLS(serverName string) *tls.Config {
	if serverName == "" {
		return sender.TLS
	}

	//nolint:exhaustruct_v5 // Preserve secure TLS defaults; only set the minimum version and verified name.
	configuration := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if sender.TLS != nil {
		configuration = sender.TLS.Clone()
		configuration.ServerName = serverName
	}

	return configuration
}
