package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"airouter/internal/domain"
)

func TestExportImportRoundTrip(t *testing.T) {
	src := testStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "p1", BaseURL: "http://a", APIKey: "k1", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "p2", BaseURL: "http://b", APIKey: "k2", Protocol: domain.ProtocolAnthropic}
	for _, p := range []*domain.Provider{p1, p2} {
		if err := src.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.CreateCombo(ctx, &domain.Combo{
		Name:     "multi",
		Strategy: domain.StrategyRoundRobin,
		Targets: []domain.ComboTarget{
			{ProviderID: p1.ID, UpstreamModel: "m1"},
			{ProviderID: p2.ID, UpstreamModel: "m2"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := src.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}

	dst := testStore(t)
	if err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := dst.GetComboByName(ctx, "multi")
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != domain.StrategyRoundRobin || len(got.Targets) != 2 {
		t.Fatalf("imported combo = %+v", got)
	}
	if got.Targets[0].Provider.Name != "p1" || got.Targets[0].UpstreamModel != "m1" {
		t.Errorf("target 0 = %+v", got.Targets[0])
	}
	if got.Targets[1].Provider.Protocol != domain.ProtocolAnthropic {
		t.Errorf("target 1 protocol = %q", got.Targets[1].Provider.Protocol)
	}
}

// TestImportLegacyComboShape accepts the pre-multi-target export format, folding
// the single provider/upstream_model fields into one target.
func TestImportLegacyComboShape(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const cfg = `{
		"version": 1,
		"providers": [{"name":"p1","base_url":"http://a","api_key":"k1","protocol":"openai"}],
		"combos": [{"name":"old","provider":"p1","upstream_model":"gpt"}]
	}`
	if err := st.Import(ctx, strings.NewReader(cfg)); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetComboByName(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != domain.StrategyFailover {
		t.Errorf("strategy = %q, want failover default", got.Strategy)
	}
	if len(got.Targets) != 1 || got.Targets[0].UpstreamModel != "gpt" || got.Targets[0].Provider.Name != "p1" {
		t.Errorf("legacy import target = %+v", got.Targets)
	}
}
