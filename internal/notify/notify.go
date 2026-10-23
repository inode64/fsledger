// Package notify delivers durable repository events through email and Slack adapters.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode/utf8"

	mail "github.com/wneessen/go-mail"

	"github.com/inode64/fsledger/internal/buildinfo"
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
	defaultSubject       = `fsledger {{.Host}} / {{.Repository}}: {{.Event}}` +
		`{{if not (or (eq .Event "error") (eq .Event "recovery"))}} ` +
		`({{.Count}}: +{{.Added}} ~{{.Modified}} -{{.Deleted}}){{end}}`
	// Up to Message.DetailLimit paths every detail is listed with its fields. Larger summaries (plugin
	// or core updates) list every path grouped by actor and change ID without per-path fields, so one
	// email carries up to digestMaxPaths paths instead of being split every few hundred. The shared
	// detection method is computed once per message. Stored evidence is unchanged either way.
	defaultBody = `{{$common := .CommonDetection}}Servidor: {{.Host}} ({{.Hostname}})
Repositorio: {{.Repository}}
{{if or (eq .Event "error") (eq .Event "recovery")}}{{if eq .Event "error"}}Estado: incidencia detectada.
{{else}}Estado: recuperado.
{{end}}{{if .ChangeID}}ID de aviso: {{.ChangeID}}
{{end}}{{if .Reason}}{{if eq .Event "recovery"}}Incidencia resuelta{{else}}Motivo{{end}}: {{.DisplayReason}}
{{end}}{{else}}Observaciones: {{.Count}} (+{{.Added}} ~{{.Modified}} -{{.Deleted}})
{{if .ChangeID}}Cambio: {{.ChangeID}}
{{end}}{{with $common}}Detección: {{.}}
{{end}}{{if .Details}}{{if .Detailed}}
Elementos cambiados
Clave  Ruta
{{range .Details}}{{printf "%-5s" .Code}}  {{printf "%q" .Path}}
{{end}}
C = Creado; D = Borrado; M = Contenido; P = Solo permisos o atributos; MP = Contenido y permisos.
T = Fecha retrocedida (mtime anterior a btime) sin otro cambio comprobado.
— = Sin evidencia suficiente para clasificar.

Detalle de cambios
{{range .Details}}
Ruta: {{printf "%q" .Path}} / {{.Code}}
{{if .ChangeID}}ID de cambio: {{.ChangeID}}
{{end}}Observado: {{localtime .Observed}}
{{if not $common}}{{with .Detection}}Detección: {{.}}
{{end}}{{end}}Actor:{{with .ActorLines}}{{range .}}
    {{.}}{{end}}{{else}} sin evidencia{{end}}
{{with .BackdatedBy}}AVISO: mtime es {{duration .}} anterior a btime, la creación de esta versión del fichero:
fecha conservada al copiar (tar, rsync -a, cp -p) o falseada.
{{end}}{{range .DisplayFields}}{{.Field}}: {{.Before}} -> {{.After}}
{{else}}Sin diferencias de campos visibles.
{{end}}{{end}}{{else}}
Elementos cambiados, agrupados por actor
Más de {{.DetailLimit}} rutas: se omite el detalle de campos por ruta; el catálogo conserva la evidencia completa.
C = Creado; D = Borrado; M = Contenido; P = Solo permisos o atributos; MP = Contenido y permisos.
T = Fecha retrocedida (mtime anterior a btime) sin otro cambio comprobado.
— = Sin evidencia suficiente para clasificar.
{{$actor := "\x00"}}{{$change := "\x00"}}{{range .Details}}
{{- if or (ne .Actor $actor) (ne .ChangeID $change)}}{{$actor = .Actor}}{{$change = .ChangeID}}
{{if .ChangeID}}ID de cambio: {{.ChangeID}}
{{end}}{{if not $common}}{{with .Detection}}Detección: {{.}}
{{end}}{{end}}Actor:{{with .ActorLines}}{{range .}}
    {{.}}{{end}}{{else}} sin evidencia{{end}}
Clave  Ruta
{{end}}{{printf "%-5s" .Code}}  {{printf "%q" .Path}}{{if .BackdatedBy}}  [mtime anterior a btime]{{end}}
{{end}}{{end}}{{end}}{{end}}`
)

var errLocalRateLimit = errors.New("slack delivery rate limited")

//nolint:gochecknoglobals // Parsed once and only executed after initialization.
var builtinTemplate = config.Template{
	Subject: template.Must(config.NotificationTemplate("notification:subject").Parse(defaultSubject)),
	Body:    template.Must(config.NotificationTemplate("notification:body").Parse(defaultBody)),
}

type renderedNotification struct {
	subject   string
	body      string
	messageID string
}

// partsLedger resumes one multipart Slack message after the prefix its outbox deliveries recorded and
// records every newly accepted prefix. Its zero value, outside the outbox, starts at the first part.
type partsLedger struct {
	record   func(catalog.Progress) error
	progress catalog.Progress
}

// Sender contains injectable clients for isolated HTTP/SMTP transport tests.
type Sender struct {
	slackEscaper *strings.Replacer
	next         map[string]time.Time
	HTTP         *http.Client
	TLS          *tls.Config
	templates    map[string]config.Template
	mutex        sync.Mutex
}

// New constructs bounded transport clients; redirects cannot leak a webhook secret.
func New(templates map[string]config.Template) *Sender {
	compiled := make(map[string]config.Template, len(templates)+1)

	maps.Copy(compiled, templates)

	compiled[""] = builtinTemplate

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

// Send returns a retry delay and sanitized failure; it never logs the destination secret. The outbox
// passes its deliveries' ledger so a multipart Slack message resumes after the parts already accepted.
func (sender *Sender) Send(
	ctx context.Context,
	notifier config.Notifier,
	message catalog.Message,
	resume *partsLedger,
) (time.Duration, error) {
	ledger := partsLedger{}
	if resume != nil {
		ledger = *resume
	}

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

	return sender.deliver(ctx, notifier, rendered, ledger)
}

// Run retries the persistent outbox independently of repository work. Each start gives the deliveries
// that stopped retrying a new retry cycle.
func (sender *Sender) Run(
	ctx context.Context,
	store *catalog.Store,
	notifiers map[string]config.Notifier,
	logger *slog.Logger,
) {
	routing := destinations(notifiers)

	retried, err := store.RetryExhausted(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		logger.Warn("outbox update failed", "error", err)
	case retried > 0:
		logger.Info("exhausted notifications retried", "repository", store.Repository, "deliveries", retried)
	}

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
	ledger partsLedger,
) (time.Duration, error) {
	switch notifier.Type {
	case config.NotifierSlack:
		delay, err := sender.reserve(ctx, notifier.URL)
		if err != nil {
			return delay, err
		}

		delay, sendErr := sender.slackParts(ctx, notifier.URL, rendered.subject+"\n"+rendered.body, ledger)
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

// finishDeliveries records one transport outcome for every delivery and returns how many stopped retrying.
func finishDeliveries(
	ctx context.Context,
	store *catalog.Store,
	deliveries []catalog.Delivery,
	delay time.Duration,
	sendErr error,
) (int, error) {
	identifiers := make([]int64, len(deliveries))
	attempts := 0

	for index, delivery := range deliveries {
		identifiers[index] = delivery.ID
		attempts = max(attempts, delivery.Attempts)
	}

	switch {
	case sendErr == nil:
		return 0, store.DeliveredMany(ctx, identifiers)
	case errors.Is(sendErr, errLocalRateLimit):
		return 0, store.DeferMany(ctx, identifiers, delay)
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
	if !exists || parsed.Subject == nil || parsed.Body == nil {
		return renderedNotification{}, fault.New("notification template unavailable")
	}

	const maxMessage = 16 << 20

	subject := limitedBuffer{Buffer: bytes.Buffer{}, remaining: maxMessage}

	err := parsed.Subject.Execute(&subject, message)
	if err != nil {
		return renderedNotification{}, fault.New("notification subject execution failed")
	}

	if strings.ContainsAny(subject.String(), "\r\n") {
		return renderedNotification{}, fault.New("notification subject contains a line break")
	}

	body := limitedBuffer{Buffer: bytes.Buffer{}, remaining: subject.remaining}

	err = parsed.Body.Execute(&body, message)
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

const (
	// emailSlogan repeats the README tagline; a test keeps the two in step.
	emailSlogan = "Detect unexpected file changes, investigate incidents, and recover recorded versions on Linux."
	projectURL  = "https://github.com/inode64/fsledger"
)

// emailFooter is the signature every email ends with, after the "-- " delimiter mail clients recognize.
func emailFooter() string {
	return "\n-- \nFSledger | " + buildinfo.Release() + "\n" + emailSlogan + "\n" + projectURL + "\n"
}

// withFooter leaves exactly one blank line between the body, however a template ends, and the signature.
func withFooter(body string) string {
	return strings.TrimRight(body, "\n") + "\n" + emailFooter()
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
	message.SetBodyString(mail.TypeTextPlain, withFooter(rendered.body))

	if rendered.messageID != "" {
		message.SetMessageIDWithValue(rendered.messageID)
	}

	smtpClient, err := client.DialToSMTPClientWithContext(ctx)
	if err != nil {
		return emailDeliveryError(err)
	}

	err = client.SendWithSMTPClient(smtpClient, message)

	// The DATA reply settles delivery before QUIT; failing an accepted message
	// here would resend it with a new Message-ID on every retry.
	closeErr := client.CloseWithSMTPClient(smtpClient)
	if closeErr != nil {
		// A failed QUIT leaves the connection open.
		slog.Warn("email connection close failed", "error", errors.Join(closeErr, smtpClient.Close()))
	}

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

	if ok && digest.Version == target.version {
		var rendered *renderedNotification

		digest, rendered = sender.digestChunk(target.notifier, digest)

		// Progress and confirmation must outlive a stop arriving after the send, or a restart sends it again.
		ledger := outboxLedger(context.WithoutCancel(ctx), store, digest.Deliveries)
		if rendered != nil {
			delay, sendErr = sender.deliver(ctx, target.notifier, *rendered, ledger)
		} else {
			delay, sendErr = sender.Send(ctx, target.notifier, digest.Message, &ledger)
		}
	} else {
		sendErr = fault.New("destination removed or changed")
	}

	// A send the stop interrupted is not a failed attempt: the restart retries it.
	if sendErr != nil && ctx.Err() != nil {
		return fault.Wrap("delivery cancelled", ctx.Err())
	}

	if sendErr != nil && !errors.Is(sendErr, errLocalRateLimit) {
		logger.Warn("notification delivery deferred", "destination", digest.Destination, "error", sendErr)
	}

	exhausted, err := finishDeliveries(context.WithoutCancel(ctx), store, digest.Deliveries, delay, sendErr)
	if err != nil {
		return err
	}

	// Logged once: the outbox keeps these deliveries, and status warns about them, but no longer retries them.
	if exhausted > 0 {
		logger.Warn(
			"notification retries exhausted; deliveries kept queued",
			"repository", digest.Message.Repository,
			"destination", digest.Destination,
			"deliveries", exhausted,
			"attempts", catalog.MaxDeliveryAttempts,
			"error", sendErr,
		)
	}

	if sendErr == nil {
		logDigest(logger, digest)
	}

	return nil
}

// outboxLedger records progress on every delivery of the digest in one transaction. The fingerprint covers
// the whole rendering, so the first delivery's progress resumes only a digest rendered exactly as before.
func outboxLedger(ctx context.Context, store *catalog.Store, deliveries []catalog.Delivery) partsLedger {
	identifiers := make([]int64, len(deliveries))
	for index, delivery := range deliveries {
		identifiers[index] = delivery.ID
	}

	return partsLedger{
		record:   func(progress catalog.Progress) error { return store.ProgressMany(ctx, identifiers, progress) },
		progress: deliveries[0].Progress,
	}
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

	return rendered, len(text)+len(emailFooter()) <= digestEmailBytes
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

// Slack messages are split without truncating the sole notification evidence. A retry of the same
// rendering resumes after the parts the ledger recorded, so no accepted part is posted again.
func (sender *Sender) slackParts(
	ctx context.Context,
	endpoint, body string,
	ledger partsLedger,
) (time.Duration, error) {
	characters := []rune(body)
	sum := sha256.Sum256([]byte(body))
	fingerprint := hex.EncodeToString(sum[:])

	start := 0
	if ledger.progress.Fingerprint == fingerprint {
		start = min(max(ledger.progress.Sent, 0), len(characters))
	}

	for offset := start; offset < len(characters); offset += slackPartSize {
		if offset > start {
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

		if end < len(characters) {
			err = sender.partAccepted(endpoint, ledger, catalog.Progress{Fingerprint: fingerprint, Sent: end})
			if err != nil {
				return 0, err
			}
		}
	}

	return 0, nil
}

// partAccepted renews the webhook reservation through the pause and the next part's own interval, so
// another message to the same webhook neither lands between two parts nor exceeds one message per
// second, and then records the accepted prefix.
func (sender *Sender) partAccepted(endpoint string, ledger partsLedger, progress catalog.Progress) error {
	sender.mutex.Lock()
	sender.next[endpoint] = time.Now().Add(slackMessageInterval + slackMessageInterval)
	sender.mutex.Unlock()

	if ledger.record == nil {
		return nil
	}

	return ledger.record(progress)
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
		// Each connection carries one message and QUIT follows; a failed RSET
		// after an accepted DATA would otherwise requeue a delivered message.
		mail.WithoutRset(),
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
