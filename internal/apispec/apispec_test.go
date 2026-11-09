package apispec

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"spawnpoint/internal/httpapi"
)

func TestEmbeddedHelpIsValidAndComplete(t *testing.T) {
	if Size() == 0 {
		t.Fatal("embedded help is empty")
	}
	if bytes.HasPrefix(Body(), []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("embedded help starts with a BOM")
	}
	if err := Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(Body(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range topLevelKeys {
		if _, ok := doc[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}
	var endpoints []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(doc["endpoints"], &endpoints); err != nil {
		t.Fatal(err)
	}
	want := []string{"process.list", "process.create", "process.get", "process.replace", "process.delete", "process.run", "process.stop", "process.restart", "process.logs", "api.help", "runtime.info"}
	if len(endpoints) != len(want) {
		t.Fatalf("endpoint count %d, want %d", len(endpoints), len(want))
	}
	for i := range want {
		if endpoints[i].ID != want[i] {
			t.Errorf("endpoint[%d] = %q, want %q", i, endpoints[i].ID, want[i])
		}
	}
}

func TestBodyReturnsACopy(t *testing.T) {
	first := Body()
	original := first[0]
	first[0] ^= 0xff
	if Body()[0] != original {
		t.Fatal("mutating Body changed the embedded asset")
	}
}

func TestValidationRejectsDamagedAssets(t *testing.T) {
	snapshot := httpapi.ContractSnapshot()
	cases := []struct {
		name     string
		body     []byte
		snapshot httpapi.Contract
	}{
		{"empty", nil, snapshot},
		{"bom", append([]byte{0xef, 0xbb, 0xbf}, Body()...), snapshot},
		{"invalid utf8", []byte{0xff, 0xfe}, snapshot},
		{"multiple json values", append(append([]byte(nil), Body()...), []byte("\n{}")...), snapshot},
		{"array top level", []byte("[]"), snapshot},
		{"missing required key", mutate(t, func(doc map[string]any) { delete(doc, "schemas") }), snapshot},
		{"duplicate endpoint", mutate(t, func(doc map[string]any) {
			endpoints := doc["endpoints"].([]any)
			endpoints = append(endpoints, endpoints[0])
			doc["endpoints"] = endpoints
		}), snapshot},
		{"implementation only route", Body(), appendRoute(snapshot, httpapi.ContractRoute{ID: "implementation.only", Path: "/implementation", Methods: []string{"GET"}, Authentication: "conditional_bearer", ResponseSchema: "help_response"})},
		{"help only route", mutate(t, func(doc map[string]any) {
			endpoints := doc["endpoints"].([]any)
			clone := cloneMap(endpoints[0].(map[string]any))
			clone["id"] = "help.only"
			clone["path"] = "/help-only"
			endpoints = append(endpoints, clone)
			doc["endpoints"] = endpoints
		}), snapshot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(tc.body, tc.snapshot); err == nil {
				t.Fatal("damaged asset passed validation")
			}
		})
	}
}

func TestRouteMismatchReportsBothValuesAndCoordinate(t *testing.T) {
	damaged := mutate(t, func(doc map[string]any) {
		endpoint := doc["endpoints"].([]any)[2].(map[string]any)
		endpoint["path"] = "/wrong/{id}"
	})

	err := validate(damaged, httpapi.ContractSnapshot())
	if err == nil {
		t.Fatal("route mismatch passed validation")
	}
	message := err.Error()
	for _, want := range []string{"area=route", "identity=process.get", "implementation=", "help="} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not contain %q", message, want)
		}
	}
}

func mutate(t *testing.T, change func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(Body(), &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func appendRoute(snapshot httpapi.Contract, route httpapi.ContractRoute) httpapi.Contract {
	snapshot.Routes = append(append([]httpapi.ContractRoute(nil), snapshot.Routes...), route)
	return snapshot
}
