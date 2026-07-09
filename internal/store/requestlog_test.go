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

func TestListRequestLogsQueryFiltersAndCursor(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	a200 := seedLog(t, st, "default", "openai", 200, "")
	b400 := seedLog(t, st, "default", "anthropic", 400, "bad request")
	c500 := seedLog(t, st, "backup", "openai", 500, "upstream down")
	_ = a200
	_ = b400
	_ = c500

	// Newest first: c500, b400, a200 (ids ascending with insert order).
	all, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all len = %d, want 3", len(all))
	}
	if all[0].ID != c500.ID || all[2].ID != a200.ID {
		t.Fatalf("order wrong: got ids %d,%d,%d", all[0].ID, all[1].ID, all[2].ID)
	}

	errs, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{StatusClass: "error", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 {
		t.Fatalf("error filter len = %d, want 2", len(errs))
	}

	ok, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{StatusClass: "ok", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(ok) != 1 || ok[0].Status != 200 {
		t.Fatalf("ok filter = %+v", ok)
	}

	byCombo, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Combo: "backup", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(byCombo) != 1 || byCombo[0].Provider != "openai" {
		t.Fatalf("combo filter = %+v", byCombo)
	}

	byProv, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Provider: "anthropic", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(byProv) != 1 || byProv[0].Status != 400 {
		t.Fatalf("provider filter = %+v", byProv)
	}

	// Cursor: page size 1, then before first page id.
	page1, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 || page1[0].ID != c500.ID {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := st.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: 1, BeforeID: page1[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].ID != b400.ID {
		t.Fatalf("page2 = %+v", page2)
	}
}

func TestDistinctRequestLogValues(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seedLog(t, st, "default", "openai", 200, "")
	seedLog(t, st, "backup", "openai", 200, "")
	seedLog(t, st, "default", "anthropic", 200, "")
	seedLog(t, st, "", "x", 200, "") // empty combo skipped

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
