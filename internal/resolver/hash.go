package resolver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// Rehash recomputes a found resolvent's cache key without the given fields
// (a type's cache_ignore_fields: labels such as name that do not change the
// series). It is called once the resolvent's configuration is known, so the
// resolver itself stays configuration-free.
func (f *Found) Rehash(ignore []string) error {
	if len(ignore) == 0 {
		return nil
	}
	trimmed := make(map[string]any, len(f.Object))
	for k, v := range f.Object {
		if !slices.Contains(ignore, k) {
			trimmed[k] = v
		}
	}
	hash, err := paramHash(f.Type, trimmed)
	if err != nil {
		return err
	}
	f.Hash = hash
	return nil
}

// paramHash builds the cache key for one resolvent object. json.Marshal of a
// decoded map sorts keys recursively, so byte-identical parameters yield the
// same hash regardless of the order they appeared in the payload.
func paramHash(resolventType string, obj map[string]any) (string, error) {
	canonical, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("hash resolvent %s: %w", resolventType, err)
	}
	h := sha256.New()
	h.Write([]byte(resolventType))
	h.Write([]byte{0}) // separator: keeps (type, params) pairs from colliding across the boundary
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}
