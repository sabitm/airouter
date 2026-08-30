package store

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"airouter/internal/domain"
)

func seedLog(t *testing.T, st *Store, combo, provider string, status int, errMsg string) *domain.RequestLog {
	t.Helper()
	l := &domain.RequestLog{
		AccessKeyName: "key",
		Combo:         combo,
		Provider:      provider,
		UpstreamModel: "m",
		Format:        "oai-chat",
		Status:        status,
		ErrMsg:        errMsg,
	}
	if err := st.CreateRequestLog(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCreateRequestLogCapsErrorMessage(t *testing.T) {
	st := testStore(t)
	message := strings.Repeat("x", maxRequestLogErrorBytes-1) + "€tail"
	log := seedLog(t, st, "default", "openai", 500, message)
	logs, err := st.ListRequestLogs(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ID != log.ID {
		t.Fatalf("logs = %+v", logs)
	}
	if len(logs[0].ErrMsg) > maxRequestLogErrorBytes {
		t.Fatalf("error length = %d, want <= %d", len(logs[0].ErrMsg), maxRequestLogErrorBytes)
	}
	if !utf8.ValidString(logs[0].ErrMsg) {
		t.Fatal("persisted error is not valid UTF-8")
	}
}

func TestListRequestLogsQueryFiltersAndPages(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	a200 := seedLog(t, st, "default", "openai", 200, "")
	b400 := seedLog(t, st, "default", "anthropic", 400, "bad request")
	c500 := seedLog(t, st, "backup", "openai", 500, "upstream down")
	_ = a200
	_ = b400
	_ = c500

	// Newest first: c500, b400, a200
	all, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: 10, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all len = %d, want 3", len(all))
	}
	if all[0].ID != c500.ID || all[2].ID != a200.ID {
		t.Fatalf("order wrong: got ids %d,%d,%d", all[0].ID, all[1].ID, all[2].ID)
	}

	errs, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{StatusClass: "error", Limit: 10, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 {
		t.Fatalf("error filter len = %d, want 2", len(errs))
	}

	n, err := st.CountRequestLogsQuery(ctx, RequestLogQuery{StatusClass: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("error count = %d, want 2", n)
	}

	page1, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: 1, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 || page1[0].ID != c500.ID {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: 1, Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].ID != b400.ID {
		t.Fatalf("page2 = %+v", page2)
	}
	page3, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: 1, Page: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 1 || page3[0].ID != a200.ID {
		t.Fatalf("page3 = %+v", page3)
	}

	total, err := st.CountRequestLogsQuery(ctx, RequestLogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
}

func TestDistinctRequestLogValues(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seedLog(t, st, "default", "openai", 200, "")
	seedLog(t, st, "backup", "openai", 200, "")
	seedLog(t, st, "default", "anthropic", 200, "")
	seedLog(t, st, "", "x", 200, "")

	combos, err := st.DistinctRequestLogValues(ctx, "combo")
	if err != nil {
		t.Fatal(err)
	}
	if len(combos) != 2 || combos[0] != "backup" || combos[1] != "default" {
		t.Fatalf("combos = %v", combos)
	}
	providers, err := st.DistinctRequestLogValues(ctx, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 3 {
		t.Fatalf("providers = %v", providers)
	}
	if _, err := st.DistinctRequestLogValues(ctx, "nope"); err == nil {
		t.Fatal("expected error for bad column")
	}
}

func TestPruneRequestLogs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	old := seedLog(t, st, "old-combo", "p", 200, "")
	recent := seedLog(t, st, "recent-combo", "p", 200, "")

	// Backdate one row's created_at so it predates the cutoff. The column has a
	// DEFAULT CURRENT_TIMESTAMP, so an explicit insert is the simplest way to
	// place a row in the past.
	oldTime := time.Now().Add(-31 * 24 * time.Hour).UTC()
	if _, err := st.db.ExecContext(ctx,
		`UPDATE request_logs SET created_at = ? WHERE id = ?`, oldTime, old.ID); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	n, err := st.PruneRequestLogs(ctx, cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}

	remaining, err := st.ListRequestLogs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != recent.ID {
		var ids []int64
		for _, l := range remaining {
			ids = append(ids, l.ID)
		}
		t.Fatalf("remaining = %v, want [%d]", ids, recent.ID)
	}
}

func TestClearRequestLogsWipesAll(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		seedLog(t, st, "combo", "prov", 200, "")
	}
	// Sanity: logs were seeded.
	got, err := st.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("seeded = %d, want 3", len(got))
	}

	if err := st.ClearRequestLogs(ctx); err != nil {
		t.Fatal(err)
	}

	got, err = st.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after clear = %d, want 0", len(got))
	}
	totalReqs, totalIn, totalOut, err := st.RequestLogStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totalReqs != 0 || totalIn != 0 || totalOut != 0 {
		t.Errorf("stats after clear = (%d,%d,%d), want all zero", totalReqs, totalIn, totalOut)
	}
}

func TestRequestLogStatsAggregates(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	// Seed logs with explicit token counts; seedLog defaults to zero.
	logs := []struct{ in, out int }{{10, 20}, {5, 7}, {3, 4}}
	for _, l := range logs {
		rl := &domain.RequestLog{
			AccessKeyName: "key",
			Combo:         "c",
			Provider:      "p",
			Format:        "oai-chat",
			Status:        200,
			InputTokens:   l.in,
			OutputTokens:  l.out,
		}
		if err := st.CreateRequestLog(ctx, rl); err != nil {
			t.Fatal(err)
		}
	}

	totalReqs, totalIn, totalOut, err := st.RequestLogStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totalReqs != 3 {
		t.Errorf("totalReqs = %d, want 3", totalReqs)
	}
	if totalIn != 18 {
		t.Errorf("totalIn = %d, want 18", totalIn)
	}
	if totalOut != 31 {
		t.Errorf("totalOut = %d, want 31", totalOut)
	}
}

func TestRequestLogStatsEmptyReturnsZero(t *testing.T) {
	st := testStore(t)
	// COALESCE must yield 0, not NULL, on an empty table.
	totalReqs, totalIn, totalOut, err := st.RequestLogStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if totalReqs != 0 || totalIn != 0 || totalOut != 0 {
		t.Errorf("empty stats = (%d,%d,%d), want all zero", totalReqs, totalIn, totalOut)
	}
}

func TestBuildLogWhere(t *testing.T) {
	cases := []struct {
		name     string
		q        RequestLogQuery
		wantSQL  string
		wantArgs []any
	}{
		{
			"empty query",
			RequestLogQuery{},
			"",
			nil,
		},
		{
			"combo only",
			RequestLogQuery{Combo: "c1"},
			" WHERE combo = ?",
			[]any{"c1"},
		},
		{
			"provider only",
			RequestLogQuery{Provider: "p1"},
			" WHERE provider = ?",
			[]any{"p1"},
		},
		{
			"status ok",
			RequestLogQuery{StatusClass: "ok"},
			" WHERE status >= 200 AND status < 300",
			nil,
		},
		{
			"status client",
			RequestLogQuery{StatusClass: "client"},
			" WHERE status >= 400 AND status < 500",
			nil,
		},
		{
			"status server",
			RequestLogQuery{StatusClass: "server"},
			" WHERE status >= 500",
			nil,
		},
		{
			"status error",
			RequestLogQuery{StatusClass: "error"},
			" WHERE status >= 400",
			nil,
		},
		{
			"whitespace padded status class trimmed",
			RequestLogQuery{StatusClass: "  ok  "},
			" WHERE status >= 200 AND status < 300",
			nil,
		},
		{
			"whitespace padded combo trimmed",
			RequestLogQuery{Combo: "  c1  "},
			" WHERE combo = ?",
			[]any{"c1"},
		},
		{
			"unknown status class adds no fragment",
			RequestLogQuery{StatusClass: "weird"},
			"",
			nil,
		},
		{
			"combo plus provider combined",
			RequestLogQuery{Combo: "c1", Provider: "p1"},
			" WHERE combo = ? AND provider = ?",
			[]any{"c1", "p1"},
		},
		{
			"combo plus status combined",
			RequestLogQuery{Combo: "c1", StatusClass: "server"},
			" WHERE combo = ? AND status >= 500",
			[]any{"c1"},
		},
		{
			"all three combined",
			RequestLogQuery{Combo: "c1", Provider: "p1", StatusClass: "ok"},
			" WHERE combo = ? AND provider = ? AND status >= 200 AND status < 300",
			[]any{"c1", "p1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSQL, gotArgs := buildLogWhere(tc.q)
			if gotSQL != tc.wantSQL {
				t.Errorf("SQL = %q, want %q", gotSQL, tc.wantSQL)
			}
			if !equalArgs(gotArgs, tc.wantArgs) {
				t.Errorf("args = %+v, want %+v", gotArgs, tc.wantArgs)
			}
		})
	}
}

func equalArgs(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestClampLogLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero returns default", 0, defaultLogLimit},
		{"negative returns default", -5, defaultLogLimit},
		{"in range passthrough", 50, 50},
		{"over max clamped", 200, maxLogLimit},
		{"exactly max", maxLogLimit, maxLogLimit},
		{"exactly default", defaultLogLimit, defaultLogLimit},
		{"one passthrough", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLogLimit(tc.in); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClampLogPage(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero returns 1", 0, 1},
		{"negative returns 1", -3, 1},
		{"positive passthrough", 5, 5},
		{"exactly 1", 1, 1},
		{"large positive", 1000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLogPage(tc.in); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
