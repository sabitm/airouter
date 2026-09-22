package web

import (
	"strings"
	"testing"

	"airouter/internal/domain"
)

func TestComboRowProviderNameTooltip(t *testing.T) {
	c := &domain.Combo{
		ID:       7,
		Name:     "default",
		Strategy: domain.StrategyFailover,
		Targets: []domain.ComboTarget{
			{
				ID:            1,
				ProviderID:    11,
				UpstreamModel: "m1",
				Enabled:       true,
				Provider: &domain.Provider{
					ID: 11, Name: "tagged", Protocol: domain.ProtocolOpenAI,
					Tags: []string{"prod", "team-a"},
				},
			},
			{
				ID:            2,
				ProviderID:    12,
				UpstreamModel: "m2",
				Enabled:       false,
				Provider: &domain.Provider{
					ID: 12, Name: "plain", Protocol: domain.ProtocolAnthropic,
				},
			},
		},
	}
	html := renderComponent(t, ComboRow(c))
	if !strings.Contains(html, `<span class="combo-provider-name" tabindex="0" title="Tags: prod, team-a">tagged</span>`) {
		t.Fatalf("missing tagged tooltip: %s", html)
	}
	if strings.Contains(html, `title="Tags: prod, team-a">plain<`) || strings.Contains(html, `tabindex="0">plain<`) {
		t.Fatalf("untagged name must stay plain: %s", html)
	}
	if !strings.Contains(html, `<span>plain</span>`) {
		t.Fatalf("missing plain provider name: %s", html)
	}
}

func TestComboProviderDropdownOmitsTagTooltip(t *testing.T) {
	ps := []*domain.Provider{{
		ID: 3, Name: "tagged", Protocol: domain.ProtocolOpenAI,
		Tags: []string{"prod"},
	}}
	html := renderComponent(t, comboTargetRow("add-0", ps, 0, "", true))
	if strings.Contains(html, "Tags:") || strings.Contains(html, `title="Tags: prod"`) {
		t.Fatalf("dropdown must not show tag tooltip: %s", html)
	}
	if !strings.Contains(html, "tagged (openai)") {
		t.Fatalf("missing provider option: %s", html)
	}
}
