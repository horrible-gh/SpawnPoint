package httpapi

import (
	"net/http"
	"reflect"
	"sort"
	"strings"

	"spawnpoint/internal/runner"
)

// Contract describes the externally visible values that the embedded help
// asset is checked against at startup. The values below are obtained from the
// live router, validation constants, status constants, error constructors and
// response JSON tags so the asset cannot silently drift from implementation.
type Contract struct {
	Routes               []ContractRoute
	PathMethods          map[string][]string
	Registration         []ContractField
	Statuses             []ContractStatus
	Errors               []ContractError
	SchemaKeys           map[string][]string
	RequestBodyMaxBytes  int
	RegistrationMaxRunes map[string]int
}

type ContractRoute struct {
	ID, Path, Authentication, ResponseSchema string
	Methods                                  []string
}

type ContractField struct {
	Name, Type         string
	Required, Nullable bool
	Default            any
	ValidationOrder    int
}

type ContractStatus struct {
	Name              string
	NullFields        []string
	ConditionalFields []string
}

type ContractError struct {
	Identity, Code, Message, Field string
	Status                         int
}

func ContractSnapshot() Contract {
	s := &Server{index: []byte{1}, help: []byte{1}}
	routes := []ContractRoute{
		contractRoute(s, "runtime.info", "/runtime", "/runtime", "runtime_response", http.MethodGet, http.MethodHead),
		contractRoute(s, "process.list", "/processes", "/processes", "list_response", http.MethodGet, http.MethodHead),
		contractRoute(s, "process.create", "/processes", "/processes", "write_response", http.MethodPost),
		contractRoute(s, "process.get", "/processes/proc_contract", "/processes/{id}", "process_response", http.MethodGet, http.MethodHead),
		contractRoute(s, "process.replace", "/processes/proc_contract", "/processes/{id}", "write_response", http.MethodPut),
		contractRoute(s, "process.delete", "/processes/proc_contract", "/processes/{id}", "delete_response", http.MethodDelete),
		contractRoute(s, "process.run", "/processes/proc_contract/run", "/processes/{id}/run", "process_response", http.MethodPost),
		contractRoute(s, "process.stop", "/processes/proc_contract/stop", "/processes/{id}/stop", "process_response", http.MethodPost),
		contractRoute(s, "process.restart", "/processes/proc_contract/restart", "/processes/{id}/restart", "process_response", http.MethodPost),
		contractRoute(s, "process.logs", "/processes/proc_contract/logs", "/processes/{id}/logs", "log_response", http.MethodGet, http.MethodHead),
		contractRoute(s, "api.help", "/help", "/help", "help_response", http.MethodGet, http.MethodHead),
	}

	invalidJSON := invalidBody(msgNotJSON)
	invalidObject := invalidBody(msgNotObject)
	cmdEmpty := invalidField("cmd", "cmd cannot be empty.")
	cmdLong := invalidField("cmd", "cmd is too long.")
	labelInvalid := invalidField("label", "label must be a string.")
	labelLong := invalidField("label", "label is too long.")
	cwdInvalid := invalidField("cwd", "cwd must be a string.")
	envInvalid := invalidField("env", "env must be an object with string keys and values.")
	autoStartInvalid := invalidField("auto_start", "auto_start must be a boolean.")
	errors := []ContractError{
		contractError("invalid_json", invalidJSON), contractError("invalid_object", invalidObject),
		contractError("cmd_empty", cmdEmpty), contractError("cmd_too_long", cmdLong),
		contractError("label_invalid", labelInvalid), contractError("label_too_long", labelLong),
		contractError("cwd_invalid", cwdInvalid), contractError("env_invalid", envInvalid),
		contractError("auto_start_invalid", autoStartInvalid),
		contractError("unauthorized", unauthorisedError()),
		contractError("process_not_found", notFoundError(msgProcessNotFound)),
		contractError("unknown_endpoint", notFoundError(msgUnknownEndpoint)),
		contractError("method_not_allowed", methodNotAllowedError()),
		contractError("payload_too_large", tooLargeError()),
		contractError("storage_error", storageError(msgInstanceStorage)),
	}

	return Contract{
		Routes: routes,
		PathMethods: map[string][]string{
			"/runtime":                append([]string(nil), s.resolve("/runtime").methods...),
			"/processes":              append([]string(nil), s.resolve("/processes").methods...),
			"/processes/{id}":         append([]string(nil), s.resolve("/processes/proc_contract").methods...),
			"/processes/{id}/run":     append([]string(nil), s.resolve("/processes/proc_contract/run").methods...),
			"/processes/{id}/stop":    append([]string(nil), s.resolve("/processes/proc_contract/stop").methods...),
			"/processes/{id}/restart": append([]string(nil), s.resolve("/processes/proc_contract/restart").methods...),
			"/processes/{id}/logs":    append([]string(nil), s.resolve("/processes/proc_contract/logs").methods...),
			"/help":                   append([]string(nil), s.resolve("/help").methods...),
		},
		Registration: []ContractField{
			{Name: "cmd", Type: "string", Required: true, Nullable: false, Default: nil, ValidationOrder: 1},
			{Name: "label", Type: "string", Required: false, Nullable: true, Default: "first_word_of_cmd", ValidationOrder: 2},
			{Name: "cwd", Type: "string", Required: false, Nullable: true, Default: nil, ValidationOrder: 3},
			{Name: "env", Type: "object<string,string>", Required: false, Nullable: true, Default: "empty_object", ValidationOrder: 4},
			{Name: "auto_start", Type: "boolean", Required: false, Nullable: false, Default: false, ValidationOrder: 5},
			{Name: "run_as", Type: "object", Required: false, Nullable: false, Default: nil, ValidationOrder: 6},
		},
		Statuses: []ContractStatus{
			{Name: runner.StatusStopped, NullFields: []string{}, ConditionalFields: []string{"pid", "exit_code", "started_at", "ended_at"}},
			{Name: runner.StatusRunning, NullFields: []string{"exit_code", "ended_at"}},
			{Name: runner.StatusExited, NullFields: []string{}},
			{Name: runner.StatusKilled, NullFields: []string{}},
			{Name: runner.StatusFailed, NullFields: []string{"pid", "exit_code", "started_at"}, ConditionalFields: []string{"error"}},
		},
		Errors: errors,
		SchemaKeys: map[string][]string{
			"runtime_response": {"ok", "os", "service_account", "run_as_supported", "run_as_reason"},
			"registration":     {"cmd", "label", "cwd", "env", "auto_start", "run_as"},
			"process":          jsonKeys(processJSON{}),
			"list_response":    jsonKeys(listResponse{}),
			"write_response":   jsonKeys(writeResponse{}),
			"process_response": jsonKeys(processResponse{}),
			"delete_response":  jsonKeys(deleteResponse{}),
			"log_response":     jsonKeys(logResponse{}),
			"error_response":   jsonKeys(errorResponse{}),
			"help_response":    {"ok", "spec_version", "service_version", "base_path", "media_type", "request_body_max_bytes", "decision_order", "authentication", "security_boundary", "success_evaluation", "process_statuses", "error_catalog", "schemas", "endpoints"},
		},
		RequestBodyMaxBytes:  RequestBodyMaxBytes,
		RegistrationMaxRunes: map[string]int{"cmd": cmdMaxLen, "label": labelMaxLen},
	}
}

func contractRoute(s *Server, id, samplePath, path, responseSchema string, methods ...string) ContractRoute {
	rt := s.resolve(samplePath)
	actual := make([]string, 0, len(methods))
	for _, method := range methods {
		if rt.allows(method) {
			actual = append(actual, method)
		}
	}
	authentication := "conditional_bearer"
	if rt.kind.authFree() {
		authentication = "none"
	}
	return ContractRoute{ID: id, Path: path, Methods: actual, Authentication: authentication, ResponseSchema: responseSchema}
}

func contractError(identity string, e apiError) ContractError {
	return ContractError{Identity: identity, Code: e.code, Status: e.status, Message: e.message, Field: e.field}
}

func jsonKeys(value any) []string {
	t := reflect.TypeOf(value)
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
}
