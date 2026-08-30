package api

import (
	"crypto/subtle"
	"net/http"
)

// authenticate checks a presented key against all configured client keys in
// constant time (every key is compared, no early exit) and returns the
// matching client's name.
func (s *Server) authenticate(presented string) (client string, ok bool) {
	p := []byte(presented)
	for _, k := range s.cfg.Auth.APIKeys {
		if subtle.ConstantTimeCompare([]byte(k.Key), p) == 1 && !ok {
			client, ok = k.Name, true
		}
	}
	return client, ok
}

// authFromHeader authenticates the caller via the X-API-Key header, writing
// a 401 and returning false on failure. Used by the authenticated GET
// handlers (job get/list/result); health endpoints stay unauthenticated.
func (s *Server) authFromHeader(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.authenticate(r.Header.Get("X-API-Key")); !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "missing or invalid X-API-Key header")
		return false
	}
	return true
}
