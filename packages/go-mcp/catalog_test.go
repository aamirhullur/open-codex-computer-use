package gomcp

import "testing"

func TestActionToolMembership(t *testing.T) {
	action := []string{"click", "drag", "perform_secondary_action", "press_key", "scroll", "set_value", "type_text"}
	for _, name := range action {
		if !IsModernActionTool(name) {
			t.Errorf("%q should be a modern action tool", name)
		}
	}
	for _, name := range []string{"get_app_state", "list_apps", "unknown"} {
		if IsModernActionTool(name) {
			t.Errorf("%q should not be a modern action tool", name)
		}
	}
	if len(actionTools) != 7 {
		t.Fatalf("action tool set size = %d, want 7", len(actionTools))
	}
}

func TestSnapshotRefProperty(t *testing.T) {
	p := SnapshotRefProperty()
	if p["type"] != "string" {
		t.Fatalf("type = %v", p["type"])
	}
	if p["description"] != SnapshotRefDescription {
		t.Fatalf("description = %v", p["description"])
	}
	// A fresh copy each call so callers cannot alias one property across tools.
	q := SnapshotRefProperty()
	q["type"] = "mutated"
	if p["type"] != "string" {
		t.Fatalf("SnapshotRefProperty returned an aliased map")
	}
}

func TestAddSnapshotRefRequirement(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"app": map[string]any{"type": "string"},
		},
		"required": []string{"app"},
	}
	AddSnapshotRefRequirement(schema)

	props := schema["properties"].(map[string]any)
	if _, ok := props[SnapshotRefKey]; !ok {
		t.Fatalf("snapshot_ref property not added: %#v", props)
	}
	req := schema["required"].([]string)
	if len(req) != 2 || req[0] != "app" || req[1] != SnapshotRefKey {
		t.Fatalf("required = %v, want [app snapshot_ref] with snapshot_ref last", req)
	}
}

func TestAddSnapshotRefRequirementMissingRequired(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	AddSnapshotRefRequirement(schema)
	req := schema["required"].([]string)
	if len(req) != 1 || req[0] != SnapshotRefKey {
		t.Fatalf("required = %v, want [snapshot_ref]", req)
	}
}
