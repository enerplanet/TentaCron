package resolver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

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
	h.Write([]byte{0})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}
