package api

import "crypto/subtle"

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
