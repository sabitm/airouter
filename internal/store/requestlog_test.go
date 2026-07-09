package store

import (
	"context"
	"testing"

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
