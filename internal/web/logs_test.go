package web

import (
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
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
