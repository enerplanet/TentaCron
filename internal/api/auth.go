package api

import (
	"crypto/subtle"
	"net/http"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
)

// identity is the authenticated caller: the key's name and role.
type identity struct {
	name string
	role string
}

func (id identity) admin() bool { return id.role == config.RoleAdmin }

// mayRead reports whether the caller may see a job: admins see every job,
// clients only the ones they submitted.
func (id identity) mayRead(j *store.Job) bool { return id.admin() || j.Client == id.name }

// authenticate checks a presented key against all configured client keys in
// constant time (every key is compared, no early exit) and returns the
// matching identity.
func (s *Server) authenticate(presented string) (identity, bool) {
	p := []byte(presented)
	var id identity
	ok := false
	for _, k := range s.cfg.Auth.APIKeys {
		if subtle.ConstantTimeCompare([]byte(k.Key), p) == 1 && !ok {
			id, ok = identity{name: k.Name, role: k.Role}, true
		}
	}
	return id, ok
}

// authFromHeader authenticates the caller via the X-API-Key header, writing
// a 401 and returning false on failure. Used by the authenticated GET
// handlers; health endpoints stay unauthenticated.
func (s *Server) authFromHeader(w http.ResponseWriter, r *http.Request) (identity, bool) {
	id, ok := s.authenticate(r.Header.Get("X-API-Key"))
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "missing or invalid X-API-Key header")
		return identity{}, false
	}
	noteClient(r, id.name)
	return id, true
}
