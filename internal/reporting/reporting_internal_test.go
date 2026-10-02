package reporting

import (
	"context"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/fault"
)

type reportFixture struct {
	draft   *catalog.ReportDraft
	failure error
	id      string
	summary string
	status  string
}

func (fixture *reportFixture) NextReport(context.Context) (*catalog.ReportDraft, error) {
	return fixture.draft, fixture.failure
}

func (fixture *reportFixture) PublishReport(_ context.Context, id, summary, status string) error {
	fixture.id, fixture.summary, fixture.status = id, summary, status
	fixture.draft = nil

	return fixture.failure
}

func TestPreparationFallbackAndNoDueReport(t *testing.T) {
	t.Parallel()

	for _, aiEnabled := range []bool{false, true} {
		fixture := &reportFixture{
			draft: &catalog.ReportDraft{
				Destinations: nil,
				Message:      catalog.Message{ChangeID: "frozen-part", Report: &catalog.ReportInfo{}},
			},
			failure: nil, id: "", summary: "", status: "",
		}

		err := prepare(t.Context(), fixture, nil, aiEnabled)
		if err != nil || fixture.id != "frozen-part" || fixture.summary != "" || fixture.draft != nil {
			t.Fatal("fallback not published", fixture, err)
		}

		if (fixture.status == "unavailable") != aiEnabled {
			t.Fatal("incorrect AI status", fixture.status)
		}

		err = prepare(t.Context(), fixture, nil, aiEnabled)
		if err != nil {
			t.Fatal("no due report should be a no-op", err)
		}

		fixture.failure = fault.New("catalog unavailable")

		err = prepare(t.Context(), fixture, nil, aiEnabled)
		if err == nil {
			t.Fatal("catalog failure swallowed")
		}
	}
}
