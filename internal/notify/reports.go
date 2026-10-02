package notify

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"text/template"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
)

const reportBody = `Informe de cambios observados
Servidor: {{printf "%q" .Host}} / repositorio: {{printf "%q" .Repository}}
Periodo (UTC): {{time .Report.From}} — {{time .Report.Until}}
Informe: {{.Report.ID}} / parte {{.Report.Part}}{{if .Report.More}} (continúa){{else}} (última){{end}}
{{if .Report.CoverageGap}}ADVERTENCIA: cobertura incompleta durante este periodo;
la ausencia de registros no demuestra ausencia de cambios.
{{end}}
Elementos cambiados
Clave  Ruta
{{range .Report.Items}}{{printf "%-5s" .Detail.Code}}  {{printf "%q" .Detail.Path}}
{{end}}
C = Creado; D = Borrado; M = Contenido; P = Solo permisos; MP = Contenido y permisos.
— = Otros metadatos o sin evidencia suficiente para clasificar.

{{if .Report.Summary}}{{.Report.Summary}}
{{else}}Resumen automático: {{len .Report.Items}} observaciones en esta parte.
{{if not .Report.Items}}No hay cambios registrados.{{end}}
{{end}}Estado IA: {{.Report.AIStatus}}
{{with .Report.Counts}}Totales de esta parte: +{{.Added}} ~{{.Modified}} -{{.Deleted}};
diferidas: {{.Deferred}}; violaciones: {{.Violations}}.
{{end}}
El detalle siguiente es evidencia independiente del resumen. Son observaciones, no necesariamente commits.
Detalle de cambios
{{range .Report.Items}}
Ruta: {{printf "%q" .Detail.Path}} / {{.Detail.Code}}
Observado: {{time .Detail.Observed}} / cambio: {{.Detail.ChangeID}}
Actor: {{printf "%q" .Detail.Actor}}
Diferido al observar: {{.Deferred}} / violación de referencia: {{.Violation}}
{{if .Head}}Último HEAD conocido al observar (no prueba de commit de esta versión): {{.Head}}
{{end}}{{range .Detail.VisibleFields}}{{.Field}}: {{printf "%q" .Before}} -> {{printf "%q" .After}}
{{else}}Sin diferencias de campos visibles.
{{end}}{{with .Detail.VisibleBaseline}}Comparación con referencia aprobada:
{{range .}}{{.Field}}: {{printf "%q" .Before}} -> {{printf "%q" .After}}
{{end}}{{end}}{{end}}`

const summaryReportBody = `Resumen de cambios observados
Servidor: {{printf "%q" .Host}} / repositorio: {{printf "%q" .Repository}}
Periodo (UTC): {{time .Report.From}} — {{time .Report.Until}}
Informe: {{.Report.ID}}
{{if .Report.CoverageGap}}ADVERTENCIA: cobertura incompleta durante este periodo;
la ausencia de registros no demuestra ausencia de cambios.
{{end}}
Observaciones: {{.Report.Aggregate.Observations}} (no necesariamente ficheros distintos).
{{with .Report.Counts}}Totales del periodo: +{{.Added}} ~{{.Modified}} -{{.Deleted}};
diferidas: {{.Deferred}}; violaciones: {{.Violations}}.
{{end}}
Muestra limitada de rutas ({{len .Report.Items}} observaciones):
{{range .Report.Items}}{{printf "%-5s" .Detail.Code}} {{printf "%q" .Detail.Path}}
{{end}}
{{if .Report.Summary}}Resumen IA de la muestra: {{.Report.Summary}}
{{end}}Estado IA: {{.Report.AIStatus}}
Los totales proceden del registro completo; la muestra no enumera todos los cambios.
`

//nolint:gochecknoglobals // Parsed once and only executed; templates permit concurrent execution.
var (
	reportTemplate        = template.Must(config.NotificationTemplate("report").Parse(reportBody))
	summaryReportTemplate = template.Must(config.NotificationTemplate("report").Parse(summaryReportBody))
)

// ReportText renders preview and delivered reports identically, without reading sources or calling AI.
func ReportText(message catalog.Message) (string, string, error) {
	if message.Report == nil {
		return "", "", fault.New("report period missing")
	}

	const maxReportBytes = 16 << 20

	body := limitedBuffer{Buffer: bytes.Buffer{}, remaining: maxReportBytes}

	compiled := reportTemplate
	if message.Report.Aggregate != nil {
		compiled = summaryReportTemplate
	}

	err := compiled.Execute(&body, message)
	if err != nil {
		return "", "", fault.New("report exceeds output limit or cannot be rendered")
	}

	subject := fmt.Sprintf(
		"fsledger %s / %s: informe de cambios (parte %d)",
		message.Host,
		message.Repository,
		message.Report.Part,
	)
	if message.Report.Aggregate != nil {
		subject = fmt.Sprintf("fsledger %s / %s: resumen de cambios", message.Host, message.Repository)
	}

	if strings.ContainsAny(subject, "\r\n") {
		return "", "", fault.New("report subject contains a line break")
	}

	return subject, body.String(), nil
}

func renderReport(message catalog.Message) (renderedNotification, error) {
	subject, body, err := ReportText(message)
	// Stable, header-safe identity survives retries without relying on mailbox deduplication.
	digest := sha256.Sum256([]byte(message.Host + "\x00" + message.Repository + "\x00" + message.ChangeID))

	return renderedNotification{
		subject:   subject,
		body:      body,
		messageID: fmt.Sprintf("%x@fsledger.invalid", digest),
	}, err
}
