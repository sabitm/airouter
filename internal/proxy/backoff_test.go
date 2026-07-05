package proxy

import (
	"testing"
	"time"

	"airouter/internal/domain"
)

func newBackoffProxy() *Proxy {
	return &Proxy{rr: map[int64]uint64{}, bo: map[int64]*backoffState{}}
}

// TestBackoffScheduleGrowsAndCaps verifies the penalty window doubles per
// consecutive failure and clamps at backoffMax.
func TestBackoffScheduleGrowsAndCaps(t *testing.T) {
	p := newBackoffProxy()
	now := time.Unix(1_000_000, 0)

	want := []time.Duration{
		backoffBase,      // 1st failure: base << 0
		backoffBase * 2,  // 2nd: base << 1
		backoffBase * 4,  // 3rd: base << 2
		backoffBase * 8,  // 4th
		backoffBase * 16, // 5th
	}
	for i, w := range want {
		p.penalizeProvider(1, now)
		got := p.bo[1].until.Sub(now)
		if got != w {
			t.Errorf("failure %d: window = %v, want %v", i+1, got, w)
		}
	}

	// Many more failures must clamp at backoffMax, never overflow.
	for i := 0; i < 40; i++ {
		p.penalizeProvider(1, now)
	}
	if got := p.bo[1].until.Sub(now); got != backoffMax {
		t.Errorf("clamped window = %v, want %v", got, backoffMax)
	}
}

// TestBackoffWindowExpiry verifies a provider is backed off only until its
// window elapses.
func TestBackoffWindowExpiry(t *testing.T) {
	p := newBackoffProxy()
	now := time.Unix(1_000_000, 0)
	p.penalizeProvider(1, now) // window = backoffBase

	if !p.providerBackedOff(1, now) {
		t.Fatal("want backed off immediately after penalty")
	}
	if !p.providerBackedOff(1, now.Add(backoffBase-time.Millisecond)) {
		t.Error("want still backed off just before window end")
	}
	if p.providerBackedOff(1, now.Add(backoffBase)) {
		t.Error("want eligible once window elapses")
	}
	if p.providerBackedOff(2, now) {
		t.Error("unrelated provider must not be backed off")
	}
}

// TestClearBackoffResets verifies a committed success drops the penalty entirely,
// so the next failure starts the schedule over at base.
func TestClearBackoffResets(t *testing.T) {
	p := newBackoffProxy()
	now := time.Unix(1_000_000, 0)
	p.penalizeProvider(1, now)
	p.penalizeProvider(1, now) // window = base*2
	p.clearBackoff(1)

	if p.providerBackedOff(1, now) {
		t.Fatal("want eligible after clear")
	}
	p.penalizeProvider(1, now)
	if got := p.bo[1].until.Sub(now); got != backoffBase {
		t.Errorf("post-clear window = %v, want base %v (schedule reset)", got, backoffBase)
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
	p.penalizeProvider(10, time.Now())

	got := p.orderTargets(combo)
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
	now := time.Now()
	p.penalizeProvider(10, now)
	p.penalizeProvider(20, now)

	got := p.orderTargets(combo)
	if len(got) != 2 || got[0].ProviderID != 10 || got[1].ProviderID != 20 {
		t.Errorf("order = %+v, want all targets in base order", got)
	}
}
