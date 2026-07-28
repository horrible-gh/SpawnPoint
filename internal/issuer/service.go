// Package issuer creates and persists SpawnPoint instances.
//
// The HTTP front end has already authenticated and validated an InstanceRequest
// before it arrives here. This package owns the rest of 0008-L 2.11 and 2.12:
// duplicate detection, the daily sequence, the random identifier tail,
// collision recovery and persistence.
package issuer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"spawnpoint/internal/dialect"
	"spawnpoint/internal/httpapi"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/store"
)

const (
	dedupWindow      = 300 * time.Second
	sequencePadWidth = 4
	randomHexDigits  = 6
	collisionRetries = 3
	sequenceLimit    = 99999999
)

// Registry is the database surface the issuing service needs.
type Registry interface {
	NextSeq(datePart string) (int, error)
	Insert(store.Instance) error
	FindActiveByKey(requestKey string, now time.Time, window time.Duration) (*store.Instance, error)
}

// Service implements httpapi.Issuer.
type Service struct {
	registry Registry
	log      *opslog.Logger

	now       func() time.Time
	randomHex func() (string, error)
}

// New builds an issuing service.
func New(registry Registry, log *opslog.Logger) *Service {
	return &Service{
		registry:  registry,
		log:       log,
		now:       time.Now,
		randomHex: secureRandomHex,
	}
}

// Issue creates an instance or returns the existing instance for a duplicate
// request key. A false result is a storage failure and is the only failure the
// HTTP seam exposes.
func (s *Service) Issue(req httpapi.InstanceRequest) (httpapi.Instance, bool) {
	now := s.now()

	if req.RequestKey != nil {
		existing, err := s.registry.FindActiveByKey(*req.RequestKey, now, dedupWindow)
		if err != nil {
			s.insertFailed("", err)
			return httpapi.Instance{}, false
		}
		if existing != nil {
			return publicInstance(*existing, true), true
		}
	}

	datePart := now.Local().Format("20060102")
	seq, err := s.registry.NextSeq(datePart)
	if err != nil {
		s.insertFailed("", err)
		return httpapi.Instance{}, false
	}
	if seq > sequenceLimit {
		s.insertFailed("", fmt.Errorf("daily sequence %d exceeds limit %d", seq, sequenceLimit))
		return httpapi.Instance{}, false
	}

	tail, err := s.randomHex()
	if err != nil {
		s.insertFailed("", err)
		return httpapi.Instance{}, false
	}
	inst := store.Instance{
		ID:         identifier(datePart, seq, tail),
		Requester:  req.Requester,
		Kind:       req.Kind,
		Status:     "created",
		RequestKey: req.RequestKey,
		Label:      req.Label,
		TTLSeconds: req.TTLSeconds,
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Duration(req.TTLSeconds) * time.Second),
	}

	for attempt := 0; ; attempt++ {
		err = s.registry.Insert(inst)
		if err == nil {
			return publicInstance(inst, false), true
		}

		writeErr, classified := dialect.AsWriteError(err)
		if !classified || writeErr.Class != dialect.DuplicateKey {
			s.warnDegraded(writeErr, classified)
			s.insertFailed(inst.ID, err)
			return httpapi.Instance{}, false
		}

		if req.RequestKey != nil {
			existing, findErr := s.registry.FindActiveByKey(*req.RequestKey, now, dedupWindow)
			if findErr != nil {
				s.insertFailed(inst.ID, findErr)
				return httpapi.Instance{}, false
			}
			if existing != nil {
				return publicInstance(*existing, true), true
			}
		}

		if attempt >= collisionRetries {
			s.insertFailed(inst.ID, err)
			return httpapi.Instance{}, false
		}
		tail, err = s.randomHex()
		if err != nil {
			s.insertFailed(inst.ID, err)
			return httpapi.Instance{}, false
		}
		inst.ID = identifier(datePart, seq, tail)
		if s.log != nil {
			s.log.Log(opslog.Warn, "instance id collision",
				opslog.F("id", inst.ID),
				opslog.F("attempt", attempt+1))
		}
	}
}

func identifier(datePart string, seq int, tail string) string {
	return fmt.Sprintf("spwn_%s_%0*d%s", datePart, sequencePadWidth, seq, tail)
}

func secureRandomHex() (string, error) {
	raw := make([]byte, randomHexDigits/2)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate instance identifier: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func publicInstance(inst store.Instance, deduplicated bool) httpapi.Instance {
	return httpapi.Instance{
		ID:           inst.ID,
		Status:       inst.Status,
		Kind:         inst.Kind,
		Requester:    inst.Requester,
		CreatedAt:    inst.CreatedAt,
		Label:        inst.Label,
		Deduplicated: deduplicated,
	}
}

func (s *Service) insertFailed(id string, err error) {
	if s.log == nil {
		return
	}
	var value any = id
	if id == "" {
		value = nil
	}
	s.log.Log(opslog.Error, "instance insert failed",
		opslog.F("id", value),
		opslog.F("detail", err))
}

func (s *Service) warnDegraded(writeErr *dialect.WriteError, ok bool) {
	if !ok || writeErr.Note != dialect.NoteExtendedCodeUnavailable || s.log == nil {
		return
	}
	s.log.Log(opslog.Warn, "extended_code_unavailable", opslog.F("dialect", dialect.Default))
}
