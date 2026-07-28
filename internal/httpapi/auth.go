package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authorised is the token check of 0008-L 4.1.
//
// An empty allow list passes everything. That is the local default and the
// current behaviour, and it is not a hole to be closed here: SpawnPoint binds
// to 127.0.0.1 unless told otherwise, and turning authentication on by default
// would lock out every existing deployment on upgrade (0007-P 0.1).
//
// No WWW-Authenticate header accompanies a rejection. Sending one makes the
// browser open its own credentials dialog over the screen, which cannot supply
// a Bearer token and cannot be dismissed back to the page.
func (s *Server) authorised(r *http.Request) bool {
	if len(s.tokens) == 0 {
		return true
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	return s.knownToken(token)
}

// bearerToken pulls the token out of an Authorization header.
//
// The scheme name is compared without case, per RFC 7235 and the current
// server. Everything else is rejected as "no token at all" rather than as a
// distinct error: a caller sending Basic credentials and a caller sending
// nothing are in the same position, and telling them apart would only describe
// the server's expectations to someone who has not authenticated.
func bearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	scheme, rest, found := strings.Cut(header, " ")
	if !found {
		return "", false
	}
	if !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}

// knownToken compares against every allowed token in constant time.
//
// A map lookup would be shorter and would leak the length of the matching
// prefix through timing. The list has a handful of entries at most, so the
// linear walk costs nothing worth measuring, and the loop deliberately does not
// stop at the first match.
func (s *Server) knownToken(token string) bool {
	found := 0
	for allowed := range s.tokens {
		found |= subtle.ConstantTimeCompare([]byte(token), []byte(allowed))
	}
	return found == 1
}
