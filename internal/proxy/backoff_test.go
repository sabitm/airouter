package proxy

import (
	"context"
	"testing"

	"airouter/internal/domain"
)

func newBackoffProxy() *Proxy {
	return &Proxy{rr: map[int64]uint64{}, bo: map[int64]*backoffState{}}
}

// TestBackoffScheduleGrowsAndCaps verifies the skip count doubles per consecutive
// failure and clamps at backoffMaxSkips.
func TestBackoffScheduleGrowsAndCaps(t *testing.T) {
	p := newBackoffProxy()

	want := []int{
		backoffBaseSkips,      // 1st failure: base << 0
		backoffBaseSkips * 2,  // 2nd: base << 1
		backoffBaseSkips * 4,  // 3rd: base << 2
		backoffBaseSkips * 8,  // 4th
		backoffBaseSkips * 16, // 5th
	}
	for i, w := range want {
		p.penalizeProvider(1)
		got := p.bo[1].skips
		if got != w {
			t.Errorf("failure %d: skips = %d, want %d", i+1, got, w)
		}
	}

	// Many more failures must clamp at backoffMaxSkips, never overflow.
	for i := 0; i < 40; i++ {
		p.penalizeProvider(1)
	}
	if got := p.bo[1].skips; got != backoffMaxSkips {
		t.Errorf("clamped skips = %d, want %d", got, backoffMaxSkips)
	}
}

// TestBackoffConsumesSkips verifies a provider is backed off for exactly as many
// requests as its skip count, consuming one credit per providerBackedOff call.
func TestBackoffConsumesSkips(t *testing.T) {
	p := newBackoffProxy()
	p.penalizeProvider(1) // skips = backoffBaseSkips

	for i := 0; i < backoffBaseSkips; i++ {
		if !p.providerBackedOff(1) {
			t.Fatalf("want backed off on consume %d of %d", i+1, backoffBaseSkips)
		}
	}
	if p.providerBackedOff(1) {
		t.Error("want eligible after all skip credits consumed")
	}
	if p.providerBackedOff(2) {
		t.Error("unrelated provider must not be backed off")
	}
}

// TestClearBackoffResets verifies a committed success drops the penalty entirely,
// so the next failure starts the schedule over at base.
func TestClearBackoffResets(t *testing.T) {
	p := newBackoffProxy()
	p.penalizeProvider(1)
	p.penalizeProvider(1) // skips = base*2
	p.clearBackoff(1)

	if p.providerBackedOff(1) {
		t.Fatal("want eligible after clear")
	}
	p.penalizeProvider(1)
	if got := p.bo[1].skips; got != backoffBaseSkips {
		t.Errorf("post-clear skips = %d, want base %d (schedule reset)", got, backoffBaseSkips)
	}
}

// TestOrderTargetsDefersBackedOff verifies a backed-off provider is moved behind
// healthy ones but never dropped, preserving relative order within each group.
func TestOrderTargetsDefersBackedOff(t *testing.T) {
	p := newBackoffProxy()
	combo := &domain.Combo{
		ID:       1,
		Strategy: domain.StrategyFailover,
		Targets: []domain.ComboTarget{
			{ProviderID: 10, Position: 0, Enabled: true},
			{ProviderID: 20, Position: 1, Enabled: true},
			{ProviderID: 30, Position: 2, Enabled: true},
		},
	}
	// Penalize the first target; it must sink to the back.
	p.penalizeProvider(10)

	got, _ := p.orderTargets(context.Background(), combo, openaiCodec, nil)
	order := [3]int64{got[0].ProviderID, got[1].ProviderID, got[2].ProviderID}
	want := [3]int64{20, 30, 10}
	if order != want {
		t.Errorf("order = %v, want %v (healthy first, penalized last)", order, want)
	}
}

// TestOrderTargetsAllBackedOffKeepsOrder verifies that when every target is
// penalized none are dropped: the combo still resolves to all targets in base
// order so the request can still attempt its least-bad option.
func TestOrderTargetsAllBackedOffKeepsOrder(t *testing.T) {
	p := newBackoffProxy()
	combo := &domain.Combo{
		ID:       1,
		Strategy: domain.StrategyFailover,
		Targets: []domain.ComboTarget{
			{ProviderID: 10, Position: 0, Enabled: true},
			{ProviderID: 20, Position: 1, Enabled: true},
		},
	}
	p.penalizeProvider(10)
	p.penalizeProvider(20)

	got, _ := p.orderTargets(context.Background(), combo, openaiCodec, nil)
	if len(got) != 2 || got[0].ProviderID != 10 || got[1].ProviderID != 20 {
		t.Errorf("order = %+v, want all targets in base order", got)
	}
}
