package qoder

import "testing"

// TestStaticModelsSyncedToReference locks the suggestion list to the reference
// registry. Suggestions never add static model_config or imply entitlement:
// chat still fails closed without a live catalog entry.
func TestStaticModelsSyncedToReference(t *testing.T) {
	want := []string{
		"auto",
		"ultimate",
		"performance",
		"efficient",
		"lite",
		"qmodel",
		"qmodel_latest",
		"qmodel_38max",
		"qfmodel",
		"kmodel_latest",
		"kmodel",
		"gmodel",
		"gfmodel",
		"dmodel",
		"dfmodel",
		"mmodel",
		"smodel",
		"cmodel",
	}
	if len(StaticModels) != len(want) {
		t.Fatalf("len = %d, want %d", len(StaticModels), len(want))
	}
	for i, key := range want {
		if StaticModels[i] != key {
			t.Fatalf("StaticModels[%d] = %q, want %q", i, StaticModels[i], key)
		}
	}
	// The retired key must not return.
	for _, key := range StaticModels {
		if key == "gm51model" {
			t.Fatal("retired gm51model still present")
		}
	}
}
