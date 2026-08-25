package web

import (
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/store"
)

func TestLogRowRendersSemanticUTCTimestamp(t *testing.T) {
	createdAt := time.Date(2026, time.August, 25, 2, 59, 2, 0, time.FixedZone("test", -4*60*60))
	html := renderComponent(t, LogRow(&domain.RequestLog{ID: 1, CreatedAt: createdAt}))

	for _, want := range []string{
		`class="log-time"`,
		`datetime="2026-08-25T06:59:02Z"`,
		`data-local-time`,
		`title="UTC: 2026-08-25 06:59:02"`,
		`<span class="log-time-date">2026-08-25</span>`,
		`<span class="log-time-clock">06:59:02 UTC</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered log row missing %q; html=%s", want, html)
		}
	}
}

func TestLogsTableUsesSevenColumnLayout(t *testing.T) {
	html := renderComponent(t, LogsTable(
		[]*domain.RequestLog{{ID: 1, CreatedAt: time.Now()}},
		store.RequestLogQuery{},
		LogsPageMeta{Page: 1, PageSize: 20, Total: 1, TotalPages: 1},
	))

	headings := []string{"Time", "Key", "Route", "Format", "Usage", "Latency", "Outcome"}
	previous := -1
	for _, heading := range headings {
		needle := ">" + heading + "</th>"
		position := strings.Index(html, needle)
		if position < 0 {
			t.Errorf("missing %s heading; html=%s", heading, html)
			continue
		}
		if position <= previous {
			t.Errorf("heading %s is out of order; html=%s", heading, html)
		}
		previous = position
	}
	for _, col := range []string{"time", "key", "route", "format", "usage", "latency", "outcome"} {
		if !strings.Contains(html, `class="log-col-`+col+`"`) {
			t.Errorf("missing named %s column; html=%s", col, html)
		}
	}
	for _, old := range []string{">Combo</th>", ">Provider / model</th>", ">Status</th>", ">Error</th>"} {
		if strings.Contains(html, old) {
			t.Errorf("found legacy heading %q; html=%s", old, html)
		}
	}
}

func TestLogRowGroupsRouteUsageLatencyAndError(t *testing.T) {
	log := &domain.RequestLog{
		ID:            42,
		CreatedAt:     time.Date(2026, time.August, 25, 2, 59, 2, 0, time.UTC),
		AccessKeyName: "(open)",
		Combo:         "default",
		Provider:      "provider-a",
		UpstreamModel: "model-a",
		Format:        "oai-chat",
		Stream:        true,
		Status:        502,
		InputTokens:   57233,
		OutputTokens:  366,
		LatencyMS:     11507,
		ErrMsg:        "upstream request failed",
	}
	html := renderComponent(t, LogRow(log))

	for _, want := range []string{
		`class="log-route-cell"`,
		`provider-a`,
		`model-a`,
		`<span class="tag">stream</span>`,
		`<strong>57,233</strong> in`,
		`<strong>366</strong> out`,
		`11.5 s`,
		`aria-controls="log-error-42"`,
		`aria-expanded="false"`,
		`id="log-error-42" class="log-error-row" hidden`,
		`colspan="7"`,
		`upstream request failed`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered log row missing %q; html=%s", want, html)
		}
	}
}

func TestLogFormattingHelpers(t *testing.T) {
	for _, tc := range []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{57233, "57,233"},
		{-1234567, "-1,234,567"},
	} {
		if got := formatLogNumber(tc.input); got != tc.want {
			t.Errorf("formatLogNumber(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}

	for _, tc := range []struct {
		input int64
		want  string
	}{
		{245, "245 ms"},
		{4087, "4.09 s"},
		{11507, "11.5 s"},
		{60000, "1m 0s"},
		{90300, "1m 30s"},
	} {
		if got := formatLogLatency(tc.input); got != tc.want {
			t.Errorf("formatLogLatency(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestTruncateErrPreservesUTF8(t *testing.T) {
	if got := truncateErr("ab界cd", 3); got != "ab界..." {
		t.Fatalf("truncateErr() = %q", got)
	}
}
