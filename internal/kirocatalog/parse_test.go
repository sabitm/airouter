package kirocatalog

import "testing"

func TestParseCapabilitySchema(t *testing.T) {
	body := []byte(`{"models":[{"modelId":"exact","additionalModelRequestFieldsSchema":{"type":"object","properties":{
		"reasoning":{"type":"object","properties":{"effort":{"enum":["low"],"default":"low"}}},
		"output_config":{"type":"object","properties":{"effort":{"enum":["low","medium","high"],"default":"medium"}}},
		"thinking":{"type":"object","properties":{"type":{"enum":["adaptive","disabled"]}}}
	}}}]}`)
	models, token, err := Parse(body)
	if err != nil || token != "" || len(models) != 1 {
		t.Fatalf("models=%v token=%q err=%v", models, token, err)
	}
	cap := models[0].Capability
	if cap == nil || cap.EffortPath != "output_config" || cap.DefaultEffort != "medium" || !cap.ThinkingToggle || !cap.ThinkingEnabled {
		t.Fatalf("cap = %#v", cap)
	}
	if len(cap.EffortLevels) != 3 || cap.EffortLevels[0] != "low" || cap.EffortLevels[2] != "high" {
		t.Fatalf("levels = %#v", cap.EffortLevels)
	}
}

func TestParseInvalidSchemas(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{name: "missing schema", body: `{"models":[{"modelId":"m"}]}`, ok: true},
		{name: "non-object", body: `{"models":[{"modelId":"m","additionalModelRequestFieldsSchema":"bad"}]}`, ok: true},
		{name: "numeric enum", body: `{"models":[{"modelId":"m","additionalModelRequestFieldsSchema":{"properties":{"output_config":{"properties":{"effort":{"enum":[1,2]}}}}}}]}`, ok: true},
		{name: "disabled default", body: `{"models":[{"modelId":"m","additionalModelRequestFieldsSchema":{"properties":{"thinking":{"properties":{"type":{"enum":["disabled","adaptive"],"default":"disabled"}}}}}}]}`, ok: true},
		{name: "no toggle", body: `{"models":[{"modelId":"m","additionalModelRequestFieldsSchema":{"properties":{"thinking":{"properties":{"type":{"enum":["enabled"]}}}}}}]}`, ok: true},
		{name: "bad model id", body: `{"models":[{"modelId":1}]}`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, _, err := Parse([]byte(tc.body))
			if tc.ok != (err == nil) {
				t.Fatalf("err=%v", err)
			}
			if !tc.ok {
				return
			}
			if tc.name == "missing schema" || tc.name == "non-object" || tc.name == "numeric enum" {
				if models[0].Capability != nil {
					t.Fatalf("guessed capability %#v", models[0].Capability)
				}
			}
			if tc.name == "disabled default" && (models[0].Capability == nil || models[0].Capability.ThinkingEnabled) {
				t.Fatalf("default disabled not preserved: %#v", models[0].Capability)
			}
			if tc.name == "no toggle" && (models[0].Capability == nil || models[0].Capability.ThinkingToggle) {
				t.Fatalf("toggle guessed: %#v", models[0].Capability)
			}
		})
	}
}
