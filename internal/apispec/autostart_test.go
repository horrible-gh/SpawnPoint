package apispec

import (
	"encoding/json"
	"testing"
)

func TestAutoStartHelpContract(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(Body(), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, field := range doc.Schemas["registration"].Fields {
		if field.Name == "auto_start" {
			found = true
			if field.Type != "boolean" || field.Required || field.Nullable || field.Default != false {
				t.Fatalf("invalid auto_start contract: %+v", field)
			}
		}
	}
	if !found {
		t.Fatal("auto_start missing from registration help")
	}
	for _, ep := range doc.Endpoints {
		if ep.ID != "process.create" && ep.ID != "process.replace" {
			continue
		}
		found = false
		for _, ref := range ep.Errors {
			if ref.CatalogID == "auto_start_invalid" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s omits the auto_start validation error", ep.ID)
		}
	}
}
