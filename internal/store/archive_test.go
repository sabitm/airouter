package store

import (
	"context"
	"testing"

	"airouter/internal/domain"
)

func TestProviderArchiveLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	pa := &domain.Provider{Name: "active", BaseURL: "http://a", APIKey: "k1", Protocol: domain.ProtocolOpenAI}
	pb := &domain.Provider{Name: "to-archive", BaseURL: "http://b", APIKey: "k2", Protocol: domain.ProtocolOpenAI}
	for _, p := range []*domain.Provider{pa, pb} {
		if err := st.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetProviderArchived(ctx, pb.ID, true); err != nil {
		t.Fatal(err)
	}

	gotB, err := st.GetProvider(ctx, pb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !gotB.Archived {
		t.Error("archived provider should have Archived == true")
	}
	gotA, err := st.GetProvider(ctx, pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.Archived {
		t.Error("active provider should have Archived == false")
	}

	all, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("list len = %d, want 2", len(all))
	}
	for _, p := range all {
		want := p.Name == "to-archive"
		if p.Archived != want {
			t.Errorf("provider %q Archived = %v, want %v", p.Name, p.Archived, want)
		}
	}

	if err := st.SetProviderArchived(ctx, pb.ID, false); err != nil {
		t.Fatal(err)
	}
	gotB, err = st.GetProvider(ctx, pb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotB.Archived {
		t.Error("restored provider should have Archived == false")
	}

	if err := st.SetProviderArchived(ctx, pb.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteArchivedProviders(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetProvider(ctx, pb.ID); err != ErrNotFound {
		t.Errorf("archived delete: got err %v, want ErrNotFound", err)
	}
	if _, err := st.GetProvider(ctx, pa.ID); err != nil {
		t.Errorf("active provider should remain: %v", err)
	}
}