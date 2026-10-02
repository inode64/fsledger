package notify

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

func TestReportRenderingPreservesEvidenceAndIdentity(t *testing.T) {
	t.Parallel()

	message := catalog.Message{
		Repository: "system", Host: "report-host", Event: catalog.ReportEvent, ChangeID: "period-1",
		Report: &catalog.ReportInfo{
			Aggregate: nil,
			ID:        "period", Part: 1, From: 1, Until: 2, More: true, CoverageGap: true,
			Summary: "Un resumen opcional", AIStatus: "summarized:test", Items: []catalog.ReportItem{
				{
					Detail: catalog.Detail{
						Path: []byte("/etc/invalid-\xff\nname"), Kind: displayModified, Actor: displayUnknown,
						ChangeID: "observed", Observed: 1, Baseline: nil,
						Fields: []catalog.FieldChange{{Field: "mode", Before: "600", After: "644"}},
					},
					Head: "last-head", Deferred: true, Violation: true,
				},
			},
		},
	}
	sender := New(nil)

	first, err := sender.render(config.Notifier{}, message)
	if err != nil {
		t.Fatal(err)
	}

	second, err := sender.render(config.Notifier{}, message)
	if err != nil || first != second || first.messageID == "" {
		t.Fatal("unstable report rendering", err)
	}

	for _, required := range []string{
		"cobertura incompleta", "continúa", "Un resumen opcional", `invalid-\xff\nname`,
		"last-head", "observed", `"600" -> "644"`,
	} {
		if !strings.Contains(first.body, required) {
			t.Fatal("missing evidence", required, first.body)
		}
	}

	message.Report.Summary = ""

	_, body, err := ReportText(message)
	if err != nil || !strings.Contains(body, "Resumen automático") {
		t.Fatal("fallback summary missing", err)
	}

	message.Host = "header\ninjection"

	_, _, err = ReportText(message)
	if err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestSummaryReportRendersCompleteTotalsAndBoundedSample(t *testing.T) {
	t.Parallel()

	message := catalog.Message{}
	message.Report = &catalog.ReportInfo{}
	message.Report.Aggregate = &catalog.ReportAggregate{}
	message.Report.Aggregate.Observations = 2000000
	message.Report.Aggregate.Counts.Added = 1999999
	message.Report.Aggregate.Counts.Violations = 1
	message.Report.CoverageGap = true

	subject, body, err := ReportText(message)
	if err != nil {
		t.Fatal(err)
	}

	for _, text := range []string{"2000000", "1999999", "violaciones: 1", "Muestra limitada", "cobertura incompleta"} {
		if !strings.Contains(body, text) {
			t.Fatal("summary lost period totals", text, body)
		}
	}

	if strings.Contains(subject, "parte") || strings.Contains(body, "No hay cambios") {
		t.Fatal("summary claimed empty inventory or used multipart wording", subject, body)
	}
}
