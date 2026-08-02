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
			{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
			{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := src.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}

	dst := testStore(t)
	if _, err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
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

func TestExportImportArchivedFlag(t *testing.T) {
	src := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{Name: "arch", BaseURL: "http://a", APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := src.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := src.SetProviderArchived(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	dst := testStore(t)
	if _, err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := dst.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived {
		t.Error("imported provider should preserve Archived == true")
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
	if _, err := st.Import(ctx, strings.NewReader(cfg)); err != nil {
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

func TestPortableComboResolveTargets(t *testing.T) {
	t.Run("new format targets returned verbatim", func(t *testing.T) {
		pc := portableCombo{
			Targets: []portableTarget{{Provider: "p1", UpstreamModel: "m1"}},
		}
		got := pc.resolveTargets()
		if len(got) != 1 || got[0].Provider != "p1" || got[0].UpstreamModel != "m1" {
			t.Errorf("got %+v, want [{p1 m1}]", got)
		}
	})

	t.Run("legacy single provider folded into one target", func(t *testing.T) {
		pc := portableCombo{Provider: "p1", UpstreamModel: "m1"}
		got := pc.resolveTargets()
		if len(got) != 1 || got[0].Provider != "p1" || got[0].UpstreamModel != "m1" {
			t.Errorf("got %+v, want [{p1 m1}]", got)
		}
	})

	t.Run("legacy with empty upstream model carried not defaulted", func(t *testing.T) {
		pc := portableCombo{Provider: "p1"}
		got := pc.resolveTargets()
		if len(got) != 1 || got[0].Provider != "p1" || got[0].UpstreamModel != "" {
			t.Errorf("got %+v, want [{p1 }]", got)
		}
	})

	t.Run("both empty returns nil", func(t *testing.T) {
		pc := portableCombo{}
		if got := pc.resolveTargets(); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})

	t.Run("empty targets slice falls through to legacy", func(t *testing.T) {
		pc := portableCombo{Targets: []portableTarget{}, Provider: "p1", UpstreamModel: "m1"}
		got := pc.resolveTargets()
		if len(got) != 1 || got[0].Provider != "p1" {
			t.Errorf("got %+v, want legacy fold (empty Targets != nil)", got)
		}
	})

	t.Run("new format targets take precedence over legacy", func(t *testing.T) {
		pc := portableCombo{
			Targets:  []portableTarget{{Provider: "new", UpstreamModel: "mn"}},
			Provider: "legacy", UpstreamModel: "ml",
		}
		got := pc.resolveTargets()
		if len(got) != 1 || got[0].Provider != "new" {
			t.Errorf("got %+v, want new-format precedence", got)
		}
	})
}

// TestImportSummaryCounts verifies create/update counters and that a single
// invalid provider is skipped without aborting the rest of the import.
func TestImportSummaryCounts(t *testing.T) {
	dst := testStore(t)
	ctx := context.Background()
	existingP := &domain.Provider{Name: "exist-p", BaseURL: "http://old", APIKey: "old", Protocol: domain.ProtocolOpenAI}
	if err := dst.CreateProvider(ctx, existingP); err != nil {
		t.Fatal(err)
	}
	if err := dst.CreateCombo(ctx, &domain.Combo{
		Name:     "exist-c",
		Strategy: domain.StrategyFailover,
		Targets:  []domain.ComboTarget{{ProviderID: existingP.ID, UpstreamModel: "old-m", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}

	const cfg = `{
		"version": 1,
		"providers": [
			{"name":"exist-p","base_url":"http://new","api_key":"k-new","protocol":"openai"},
			{"name":"new-p","base_url":"http://b","api_key":"k2","protocol":"anthropic"},
			{"name":"bad-p","base_url":"http://c","api_key":"k3","protocol":"not-a-protocol"}
		],
		"combos": [
			{"name":"exist-c","strategy":"roundrobin","targets":[{"provider":"exist-p","upstream_model":"m1"},{"provider":"new-p","upstream_model":"m2"}]},
			{"name":"new-c","strategy":"failover","targets":[{"provider":"new-p","upstream_model":"m3"}]}
		]
	}`
	sum, err := dst.Import(ctx, strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if sum.ProvidersCreated != 1 || sum.ProvidersUpdated != 1 || sum.CombosCreated != 1 || sum.CombosUpdated != 1 {
		t.Fatalf("summary counts = createdP=%d updatedP=%d createdC=%d updatedC=%d, want 1/1/1/1",
			sum.ProvidersCreated, sum.ProvidersUpdated, sum.CombosCreated, sum.CombosUpdated)
	}
	if len(sum.Failures) != 1 {
		t.Fatalf("Failures = %v, want 1", sum.Failures)
	}
	if !strings.Contains(sum.Failures[0], `provider "bad-p"`) {
		t.Errorf("failure = %q, want bad-p", sum.Failures[0])
	}
}

// TestImportFailureSkipKeepsOthers confirms validation failures skip only the
// bad row; good providers and combos that follow still commit.
func TestImportFailureSkipKeepsOthers(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const cfg = `{
		"version": 1,
		"providers": [
			{"name":"p1","base_url":"http://a","api_key":"k1","protocol":"openai"},
			{"name":"bad","base_url":"http://x","api_key":"kx","protocol":"nope"},
			{"name":"p2","base_url":"http://b","api_key":"k2","protocol":"anthropic"}
		],
		"combos": [
			{"name":"c1","targets":[{"provider":"p1","upstream_model":"m1"},{"provider":"p2","upstream_model":"m2"}]}
		]
	}`
	sum, err := st.Import(ctx, strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if sum.ProvidersCreated != 2 || len(sum.Failures) != 1 || sum.CombosCreated != 1 {
		t.Fatalf("summary = %+v, want createdP=2 failures=1 createdC=1", sum)
	}
	if _, err := st.GetComboByName(ctx, "c1"); err != nil {
		t.Fatalf("combo missing: %v", err)
	}
	ps, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("providers = %d, want 2", len(ps))
	}
	names := map[string]bool{}
	for _, p := range ps {
		names[p.Name] = true
	}
	if !names["p1"] || !names["p2"] {
		t.Errorf("provider names = %v, want p1 and p2", names)
	}
}

// TestImportDecodeError aborts before any write when the JSON is unreadable.
func TestImportDecodeError(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	sum, err := st.Import(ctx, strings.NewReader(`{"version":1,"providers":[`))
	if err == nil {
		t.Fatal("want decode error")
	}
	if sum != nil {
		t.Errorf("summary = %+v, want nil on decode error", sum)
	}
	ps, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Fatalf("providers = %d, want 0 after aborted decode", len(ps))
	}
}
