package main

import (
	"reflect"
	"testing"
)

// TestModelsConfigIDAndRoleSet pins the configured model set and roles. The
// factory no longer uses two of the models the catalog lists (old issue #3),
// and a later edit must not quietly bring one back or move a seat's role. The
// expected values are the assertion itself; everything else about a model
// (prices, max_concurrent, context) is free to follow the catalog.
func TestModelsConfigIDAndRoleSet(t *testing.T) {
	want := map[string]string{
		"zai-org/glm-5.3-flash":                "base",
		"deepseek-ai/deepseek-v4.1-flash-1017": "mid",
		"moonshotai/kimi-k3.1-pro":             "top",
		"zai-org/glm-5.3":                      "experiment",
		"moonshotai/kimi-k3":                   "experiment",
		"openai/gpt-oss-120b":                  "experiment",
		"zai-org/glm-4.7-flash":                "experiment",
	}
	got := make(map[string]string, len(want))
	for _, m := range testModels(t) {
		if _, dup := got[m.ID]; dup {
			t.Errorf("models.json has a duplicate id %q", m.ID)
		}
		got[m.ID] = m.Role
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("models.json id/role set = %v, want %v", got, want)
	}
}
