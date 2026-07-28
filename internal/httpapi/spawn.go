package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Instance request limits (0008-L 1.1).
const (
	requesterMaxLen = 64
	kindMaxLen      = 32
	// spawnLabelMaxLen is 256 because 0007-P 0.5 and 0008-L 1.1 both say 256.
	// Migration 003 brings the inherited 128-character database constraint
	// into agreement with that external contract.
	spawnLabelMaxLen = 256
	ttlMin           = 60
	ttlMax           = 86400
	// TTLDefault applies when options.ttl_seconds is absent.
	TTLDefault = 3600
)

// allowedKinds is compared by exact equality. Order is irrelevant and case is
// not folded — `Worker` is not `worker` (0008-L 1.1).
var allowedKinds = []string{"session", "worker", "task"}

// InstanceRequest is a validated POST /spawn body.
//
// It is the seam between this package and the issuing service. Everything the
// request front end owns — the body limit, authentication, the JSON shape, the
// four field checks and their order — happens before one of these is built;
// everything the issuer owns — the daily sequence, the identifier, the
// duplicate window, the store — happens after. Neither side re-does the other's
// work.
type InstanceRequest struct {
	Requester string
	Kind      string
	// RequestKey is nil when the caller sent none, which switches duplicate
	// detection off entirely rather than making every keyless request a
	// duplicate of the last one (0007-P [인스턴스 발급 — 중복 요청]).
	RequestKey *string
	// Label is nil when absent. The distinction reaches the response: a missing
	// label means the key is not there at all, not that it is null.
	Label *string
	// TTLSeconds is already resolved to its default when the caller omitted it.
	TTLSeconds int
}

// Instance is what an issuer produces. The fields are exactly those 0007-P puts
// on the wire — expires_at, ttl_seconds and request_key are stored and not
// reported, which is the current contract and is not widened here.
type Instance struct {
	ID        string
	Status    string
	Kind      string
	Requester string
	CreatedAt time.Time
	Label     *string
	// Deduplicated marks an instance that already existed. It reaches the
	// response as a key that is only present when true.
	Deduplicated bool
}

// Issuer creates instances. [T7]
//
// A false second result is a storage failure and nothing else: an identifier
// collision is resolved behind this interface — by returning the existing
// instance when a request key names one, or by redrawing the random tail — so
// it never arrives here as an error to be interpreted (0008-L 4.4).
type Issuer interface {
	Issue(req InstanceRequest) (Instance, bool)
}

// instanceJSON is the `instance` block of 0007-P [인스턴스 발급 — 정상].
type instanceJSON struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Kind      string `json:"kind"`
	Requester string `json:"requester"`
	CreatedAt string `json:"created_at"`
	// Label is omitted, not nulled, when the request carried none.
	Label *string `json:"label,omitempty"`
}

// spawnResponse puts `deduplicated` ahead of `instance`, as the contract shows,
// and omits it when the instance is new. `false` is never sent: a caller that
// sees the key at all knows it got something that already existed.
type spawnResponse struct {
	OK           bool         `json:"ok"`
	Deduplicated bool         `json:"deduplicated,omitempty"`
	Instance     instanceJSON `json:"instance"`
}

// validateSpawn is 0008-L 4.1's order for /spawn: requester, kind,
// options.label, options.ttl_seconds.
//
// The label limit is 256 throughout the request and storage path. Migration 003
// widens the old 128-character database constraint without touching existing
// rows.
func validateSpawn(obj map[string]json.RawMessage) (InstanceRequest, *apiError) {
	var out InstanceRequest

	raw, ok := present(obj, "requester")
	if !ok {
		return out, ptr(invalidField("requester", "requester cannot be empty."))
	}
	requester, ok := asString(raw)
	if !ok || strings.TrimSpace(requester) == "" {
		return out, ptr(invalidField("requester", "requester cannot be empty."))
	}
	if utf8.RuneCountInString(requester) > requesterMaxLen {
		return out, ptr(invalidField("requester", "requester is too long."))
	}
	out.Requester = requester

	raw, ok = present(obj, "kind")
	if !ok {
		return out, ptr(invalidField("kind", "kind cannot be empty."))
	}
	kind, ok := asString(raw)
	if !ok || strings.TrimSpace(kind) == "" {
		return out, ptr(invalidField("kind", "kind cannot be empty."))
	}
	if utf8.RuneCountInString(kind) > kindMaxLen {
		return out, ptr(invalidField("kind", "kind is too long."))
	}
	if !allowedKind(kind) {
		return out, ptr(invalidField("kind", "kind is not allowed."))
	}
	out.Kind = kind

	if raw, ok := present(obj, "request_key"); ok {
		key, ok := asString(raw)
		if !ok {
			// Not a field 0007-P gives a message for, because the current
			// implementation does not check it: a non-string key would be
			// stored as whatever it stringified to. Refusing it is the smaller
			// surprise, and it uses the shape every other complaint uses.
			return out, ptr(invalidField("request_key", "request_key must be a string."))
		}
		out.RequestKey = &key
	}

	options := map[string]json.RawMessage{}
	if raw, ok := present(obj, "options"); ok {
		if err := json.Unmarshal(raw, &options); err != nil {
			// 0007-P names no message for this because the current server has
			// none: it reads `options` straight into a dict lookup and raises,
			// which reaches the caller as a dropped connection. A 400 naming
			// the field is the same answer every other malformed item gets.
			return out, ptr(invalidField("options", "options must be an object."))
		}
	}

	if raw, ok := present(options, "label"); ok {
		label, ok := asString(raw)
		if !ok || utf8.RuneCountInString(label) > spawnLabelMaxLen {
			return out, ptr(invalidField("options.label", "label is too long."))
		}
		out.Label = &label
	}

	out.TTLSeconds = TTLDefault
	if raw, ok := present(options, "ttl_seconds"); ok {
		ttl, ok := asInteger(raw)
		if !ok || ttl < ttlMin || ttl > ttlMax {
			// One message for both "not a number" and "out of range". That is
			// the current contract (0007-P shows `true` producing exactly this),
			// and splitting it would change a response two documents pin.
			return out, ptr(invalidField("options.ttl_seconds", "ttl_seconds is outside the allowed range."))
		}
		out.TTLSeconds = int(ttl)
	}
	return out, nil
}

func allowedKind(kind string) bool {
	for _, k := range allowedKinds {
		if k == kind {
			return true
		}
	}
	return false
}

func (s *Server) serveSpawn(w http.ResponseWriter, r *http.Request) {
	obj, apiErr := readObject(r)
	if apiErr != nil {
		s.fail(w, r, *apiErr)
		return
	}
	req, apiErr := validateSpawn(obj)
	if apiErr != nil {
		s.fail(w, r, *apiErr)
		return
	}
	if s.issuer == nil {
		// The route is defined and the request is good; there is simply nothing
		// behind it yet. A storage error is the honest answer — the request was
		// accepted and could not be stored — and it is the one code 0007-P
		// gives for a /spawn that fails after validation.
		s.fail(w, r, storageError(msgInstanceStorage))
		return
	}
	inst, ok := s.issuer.Issue(req)
	if !ok {
		s.fail(w, r, storageError(msgInstanceStorage))
		return
	}
	s.writeJSON(w, r, http.StatusOK, spawnResponse{
		OK:           true,
		Deduplicated: inst.Deduplicated,
		Instance: instanceJSON{
			ID:        inst.ID,
			Status:    inst.Status,
			Kind:      inst.Kind,
			Requester: inst.Requester,
			CreatedAt: responseTime(inst.CreatedAt),
			Label:     inst.Label,
		},
	})
}
