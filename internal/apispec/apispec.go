// Package apispec embeds and validates SpawnPoint's machine-readable API help.
package apispec

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"unicode/utf8"

	"spawnpoint/internal/httpapi"
)

//go:embed help.json
var body []byte

func Body() []byte { return append([]byte(nil), body...) }

func Size() int { return len(body) }

func Validate() error { return validate(body, httpapi.ContractSnapshot()) }

type document struct {
	OK                  bool                    `json:"ok"`
	SpecVersion         string                  `json:"spec_version"`
	ServiceVersion      string                  `json:"service_version"`
	BasePath            string                  `json:"base_path"`
	MediaType           string                  `json:"media_type"`
	RequestBodyMaxBytes int                     `json:"request_body_max_bytes"`
	DecisionOrder       []string                `json:"decision_order"`
	Authentication      authentication          `json:"authentication"`
	SecurityBoundary    securityBoundary        `json:"security_boundary"`
	SuccessEvaluation   successEvaluation       `json:"success_evaluation"`
	ProcessStatuses     []statusRecord          `json:"process_statuses"`
	ErrorCatalog        []errorRecord           `json:"error_catalog"`
	Schemas             map[string]schemaRecord `json:"schemas"`
	Endpoints           []endpointRecord        `json:"endpoints"`
}

type authentication struct {
	Mode                        string   `json:"mode"`
	DefaultEnabled              bool     `json:"default_enabled"`
	EnabledWhen                 string   `json:"enabled_when"`
	Header                      string   `json:"header"`
	AuthFreePaths               []string `json:"auth_free_paths"`
	HelpRequiresAuthWhenEnabled bool     `json:"help_requires_auth_when_enabled"`
}

type securityBoundary struct {
	DefaultBind                               string `json:"default_bind"`
	ArbitraryShellCommandExecution            bool   `json:"arbitrary_shell_command_execution"`
	RemoteBindingExposesAPI                   bool   `json:"remote_binding_exposes_api"`
	RemoteBindingRequiresAuthentication       bool   `json:"remote_binding_requires_authentication"`
	RemoteBindingRequiresNetworkAccessControl bool   `json:"remote_binding_requires_network_access_control"`
}

type successEvaluation struct {
	HTTPStatusOnlySufficient bool         `json:"http_status_only_sufficient"`
	Rules                    []ruleRecord `json:"rules"`
}

type ruleRecord struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type statusRecord struct {
	Name              string   `json:"name"`
	NullFields        []string `json:"null_fields"`
	ConditionalFields []string `json:"conditional_fields"`
}

type errorRecord struct {
	ID             string  `json:"id"`
	Code           string  `json:"code"`
	HTTPStatus     int     `json:"http_status"`
	Message        string  `json:"message"`
	Field          *string `json:"field"`
	FieldCondition string  `json:"field_condition"`
}

type schemaRecord struct {
	Keys   []string      `json:"keys"`
	Fields []schemaField `json:"fields"`
}

type schemaField struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	Required        bool   `json:"required"`
	Nullable        bool   `json:"nullable"`
	Default         any    `json:"default"`
	ValidationOrder int    `json:"validation_order"`
	MaxRunes        *int   `json:"max_runes"`
}

type endpointRecord struct {
	ID             string          `json:"id"`
	Methods        []string        `json:"methods"`
	Path           string          `json:"path"`
	Authentication string          `json:"authentication"`
	Request        requestRecord   `json:"request"`
	Success        successRecord   `json:"success"`
	Errors         []endpointError `json:"errors"`
	SideEffects    sideEffects     `json:"side_effects"`
	Idempotency    idempotency     `json:"idempotency"`
}

type requestRecord struct {
	Headers         []string `json:"headers"`
	PathParameters  []string `json:"path_parameters"`
	QueryParameters []string `json:"query_parameters"`
	BodySchema      *string  `json:"body_schema"`
	BodyRequired    bool     `json:"body_required"`
}

type successRecord struct {
	HTTPStatus       int      `json:"http_status"`
	ResponseSchema   string   `json:"response_schema"`
	JudgementRuleIDs []string `json:"judgement_rule_ids"`
}

type endpointError struct {
	CatalogID string `json:"catalog_id"`
}

type sideEffects struct {
	Process  string `json:"process"`
	Log      string `json:"log"`
	Database string `json:"database"`
}

type idempotency struct {
	Class          string  `json:"class"`
	RequestKey     *string `json:"request_key"`
	AutomaticRetry bool    `json:"automatic_retry"`
	RetryRule      string  `json:"retry_rule"`
}

var topLevelKeys = []string{"ok", "spec_version", "service_version", "base_path", "media_type", "request_body_max_bytes", "decision_order", "authentication", "security_boundary", "success_evaluation", "process_statuses", "error_catalog", "schemas", "endpoints"}
var ruleIDs = []string{"require_2xx_and_ok", "require_persisted_for_durability", "failed_status_means_start_failure_even_on_200", "require_running_when_running_is_the_goal", "delete_persisted_false_may_revive_after_restart"}

func validate(raw []byte, snapshot httpapi.Contract) error {
	if len(raw) == 0 {
		return mismatch("format", "body", "non-empty", "empty")
	}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		return mismatch("format", "bom", "absent", "present")
	}
	if !utf8.Valid(raw) {
		return mismatch("format", "utf8", "valid", "invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var top map[string]json.RawMessage
	if err := decoder.Decode(&top); err != nil {
		return fmt.Errorf("API help format: %w", err)
	}
	if top == nil {
		return mismatch("format", "top_level", "object", "non-object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return mismatch("format", "json_values", 1, 2)
		}
		return fmt.Errorf("API help trailing data: %w", err)
	}
	for _, key := range topLevelKeys {
		if _, ok := top[key]; !ok {
			return mismatch("top_level", key, "present", "missing")
		}
	}
	types := map[string]string{
		"ok": "boolean", "spec_version": "string", "service_version": "string", "base_path": "string", "media_type": "string",
		"request_body_max_bytes": "number", "decision_order": "array", "authentication": "object", "security_boundary": "object",
		"success_evaluation": "object", "process_statuses": "array", "error_catalog": "array", "schemas": "object", "endpoints": "array",
	}
	for key, want := range types {
		if got := jsonKind(top[key]); got != want {
			return mismatch("top_level_type", key, want, got)
		}
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("API help decode: %w", err)
	}
	if !doc.OK {
		return mismatch("fixed", "ok", true, doc.OK)
	}
	if doc.SpecVersion != "2026-08-11.1" {
		return mismatch("fixed", "spec_version", "2026-08-11.1", doc.SpecVersion)
	}
	if doc.ServiceVersion != httpapi.Version {
		return mismatch("fixed", "service_version", httpapi.Version, doc.ServiceVersion)
	}
	if doc.BasePath != "/" {
		return mismatch("fixed", "base_path", "/", doc.BasePath)
	}
	if doc.MediaType != "application/json; charset=utf-8" {
		return mismatch("fixed", "media_type", "application/json; charset=utf-8", doc.MediaType)
	}
	if doc.RequestBodyMaxBytes != snapshot.RequestBodyMaxBytes {
		return mismatch("limit", "request_body_max_bytes", snapshot.RequestBodyMaxBytes, doc.RequestBodyMaxBytes)
	}
	if !reflect.DeepEqual(doc.DecisionOrder, []string{"body_size", "path", "method", "authentication", "json", "fields"}) {
		return mismatch("fixed", "decision_order", []string{"body_size", "path", "method", "authentication", "json", "fields"}, doc.DecisionOrder)
	}
	if err := validateAuthentication(doc.Authentication); err != nil {
		return err
	}
	if err := validateSecurity(doc.SecurityBoundary); err != nil {
		return err
	}
	if err := validateRules(doc.SuccessEvaluation); err != nil {
		return err
	}
	if err := validateEndpoints(top["endpoints"], doc, snapshot); err != nil {
		return err
	}
	if err := validateSchemas(doc, snapshot); err != nil {
		return err
	}
	if err := validateStatuses(doc.ProcessStatuses, snapshot.Statuses); err != nil {
		return err
	}
	if err := validateErrors(doc.ErrorCatalog, snapshot.Errors); err != nil {
		return err
	}
	return nil
}

func validateAuthentication(got authentication) error {
	wantPaths := []string{"/", "/index.html", "/healthz"}
	if got.Mode != "conditional_bearer" || got.DefaultEnabled || got.EnabledWhen != "SPAWNPOINT_API_TOKENS is non-empty" || got.Header != "Authorization: Bearer <token>" || !reflect.DeepEqual(got.AuthFreePaths, wantPaths) || !got.HelpRequiresAuthWhenEnabled {
		return mismatch("authentication", "conditional_bearer", "fixed authentication contract", got)
	}
	return nil
}

func validateSecurity(got securityBoundary) error {
	if got.DefaultBind != "127.0.0.1:8091" || !got.ArbitraryShellCommandExecution || !got.RemoteBindingExposesAPI || !got.RemoteBindingRequiresAuthentication || !got.RemoteBindingRequiresNetworkAccessControl {
		return mismatch("security_boundary", "remote_binding", "fixed security boundary", got)
	}
	return nil
}

func validateRules(got successEvaluation) error {
	if got.HTTPStatusOnlySufficient {
		return mismatch("success_evaluation", "http_status_only_sufficient", false, true)
	}
	ids := make([]string, len(got.Rules))
	for i, rule := range got.Rules {
		ids[i] = rule.ID
	}
	if !reflect.DeepEqual(ids, ruleIDs) {
		return mismatch("success_evaluation", "rules", ruleIDs, ids)
	}
	return nil
}

func validateEndpoints(raw json.RawMessage, doc document, snapshot httpapi.Contract) error {
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		return fmt.Errorf("endpoint records: %w", err)
	}
	required := map[string]string{"id": "string", "methods": "array", "path": "string", "authentication": "string", "request": "object", "success": "object", "errors": "array", "side_effects": "object", "idempotency": "object"}
	seen := map[string]endpointRecord{}
	rules := map[string]bool{}
	for _, id := range ruleIDs {
		rules[id] = true
	}
	for i, record := range records {
		for key, kind := range required {
			value, ok := record[key]
			if !ok {
				return mismatch("endpoint_field", fmt.Sprintf("index=%d.%s", i, key), "present", "missing")
			}
			if got := jsonKind(value); got != kind {
				return mismatch("endpoint_field_type", fmt.Sprintf("index=%d.%s", i, key), kind, got)
			}
		}
		ep := doc.Endpoints[i]
		if ep.ID == "" {
			return mismatch("endpoint", fmt.Sprintf("index=%d.id", i), "non-empty", "empty")
		}
		if _, ok := seen[ep.ID]; ok {
			return mismatch("endpoint", ep.ID, "unique", "duplicate")
		}
		seen[ep.ID] = ep
		methods := map[string]bool{}
		for _, method := range ep.Methods {
			if method != http.MethodGet && method != http.MethodHead && method != http.MethodPost && method != http.MethodPut && method != http.MethodDelete {
				return mismatch("endpoint_method", ep.ID, "standard uppercase method", method)
			}
			if methods[method] {
				return mismatch("endpoint_method", ep.ID, "unique", method)
			}
			methods[method] = true
		}
		if _, ok := doc.Schemas[ep.Success.ResponseSchema]; !ok {
			return mismatch("endpoint_schema", ep.ID, "defined", ep.Success.ResponseSchema)
		}
		if ep.Request.BodySchema != nil {
			if _, ok := doc.Schemas[*ep.Request.BodySchema]; !ok {
				return mismatch("endpoint_schema", ep.ID, "defined", *ep.Request.BodySchema)
			}
		}
		for _, id := range ep.Success.JudgementRuleIDs {
			if !rules[id] {
				return mismatch("endpoint_rule", ep.ID, "defined", id)
			}
		}
		for _, ref := range ep.Errors {
			if !hasError(doc.ErrorCatalog, ref.CatalogID) {
				return mismatch("endpoint_error", ep.ID, "defined", ref.CatalogID)
			}
		}
	}
	implementation := map[string]httpapi.ContractRoute{}
	for _, route := range snapshot.Routes {
		implementation[route.ID] = route
	}
	for id, route := range implementation {
		ep, ok := seen[id]
		if !ok {
			return mismatch("route", id, route, "missing from help")
		}
		if route.Path != ep.Path || route.Authentication != ep.Authentication || route.ResponseSchema != ep.Success.ResponseSchema || !sameStrings(route.Methods, ep.Methods) {
			return mismatch("route", id, route, ep)
		}
	}
	for id, ep := range seen {
		if _, ok := implementation[id]; !ok {
			return mismatch("route", id, "missing from implementation", ep)
		}
	}
	helpPathMethods := map[string][]string{}
	for _, ep := range doc.Endpoints {
		helpPathMethods[ep.Path] = append(helpPathMethods[ep.Path], ep.Methods...)
	}
	for path, methods := range snapshot.PathMethods {
		if !sameStrings(methods, helpPathMethods[path]) {
			return mismatch("route_methods", path, sorted(methods), sorted(helpPathMethods[path]))
		}
	}
	for path, methods := range helpPathMethods {
		if _, ok := snapshot.PathMethods[path]; !ok {
			return mismatch("route_methods", path, "missing from implementation", sorted(methods))
		}
	}
	for _, id := range []string{"process.create", "process.restart"} {
		ep := seen[id]
		wantRule := "do_not_retry_after_ambiguous_result"
		if id == "process.restart" {
			wantRule = "retry_only_when_another_restart_is_explicitly_accepted"
		}
		if ep.Idempotency.RequestKey != nil || ep.Idempotency.AutomaticRetry || ep.Idempotency.RetryRule != wantRule {
			return mismatch("idempotency", id, fmt.Sprintf("request_key=null automatic_retry=false retry_rule=%s", wantRule), ep.Idempotency)
		}
	}
	return nil
}

func validateSchemas(doc document, snapshot httpapi.Contract) error {
	for name, want := range snapshot.SchemaKeys {
		schema, ok := doc.Schemas[name]
		if !ok {
			return mismatch("schema", name, want, "missing")
		}
		if !sameStrings(want, schema.Keys) {
			return mismatch("schema_keys", name, sorted(want), sorted(schema.Keys))
		}
	}
	for name := range doc.Schemas {
		if _, ok := snapshot.SchemaKeys[name]; !ok {
			return mismatch("schema", name, "missing from implementation", "help only")
		}
	}
	registration := doc.Schemas["registration"]
	if len(registration.Fields) != len(snapshot.Registration) {
		return mismatch("registration", "field_count", len(snapshot.Registration), len(registration.Fields))
	}
	for i, want := range snapshot.Registration {
		got := registration.Fields[i]
		if want.Name != got.Name || want.Type != got.Type || want.Required != got.Required || want.Nullable != got.Nullable || want.ValidationOrder != got.ValidationOrder || !reflect.DeepEqual(want.Default, got.Default) {
			return mismatch("registration", want.Name, want, got)
		}
		if max, ok := snapshot.RegistrationMaxRunes[want.Name]; ok {
			if got.MaxRunes == nil || *got.MaxRunes != max {
				return mismatch("registration_limit", want.Name, max, got.MaxRunes)
			}
		} else if got.MaxRunes != nil {
			return mismatch("registration_limit", want.Name, nil, *got.MaxRunes)
		}
	}
	return nil
}

func validateStatuses(got []statusRecord, want []httpapi.ContractStatus) error {
	if len(got) != len(want) {
		return mismatch("statuses", "count", len(want), len(got))
	}
	for i := range want {
		if got[i].Name != want[i].Name || !sameStrings(got[i].NullFields, want[i].NullFields) || !sameStrings(got[i].ConditionalFields, want[i].ConditionalFields) {
			return mismatch("status", want[i].Name, want[i], got[i])
		}
	}
	return nil
}

func validateErrors(got []errorRecord, want []httpapi.ContractError) error {
	if len(got) != len(want) {
		return mismatch("errors", "count", len(want), len(got))
	}
	seen := map[string]errorRecord{}
	for _, record := range got {
		if _, ok := seen[record.ID]; ok {
			return mismatch("error", record.ID, "unique", "duplicate")
		}
		seen[record.ID] = record
	}
	for _, expected := range want {
		record, ok := seen[expected.Identity]
		if !ok {
			return mismatch("error", expected.Identity, expected, "missing")
		}
		field := ""
		if record.Field != nil {
			field = *record.Field
		}
		if record.Code != expected.Code || record.HTTPStatus != expected.Status || record.Message != expected.Message || field != expected.Field || record.FieldCondition == "" {
			return mismatch("error", expected.Identity, expected, record)
		}
	}
	for id := range seen {
		found := false
		for _, expected := range want {
			if expected.Identity == id {
				found = true
				break
			}
		}
		if !found {
			return mismatch("error", id, "missing from implementation", "help only")
		}
	}
	return nil
}

func hasError(records []errorRecord, id string) bool {
	for _, record := range records {
		if record.ID == id {
			return true
		}
	}
	return false
}

func jsonKind(raw json.RawMessage) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "invalid"
	}
	switch value.(type) {
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

func sameStrings(a, b []string) bool { return reflect.DeepEqual(sorted(a), sorted(b)) }
func sorted(in []string) []string    { out := append([]string(nil), in...); sort.Strings(out); return out }
func mismatch(area, identity string, implementation, help any) error {
	return fmt.Errorf("API help contract mismatch: area=%s identity=%s implementation=%v help=%v", area, identity, implementation, help)
}
