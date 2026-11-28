package util

import (
	"crypto/sha256"
	"encoding/hex"
)

// SHA256Hex returns the hex-encoded SHA-256 digest of s.
//
// Used to fingerprint a requirement's design plan so a child requirement can
// tell whether the master plan it was designed against has since changed
// (see requirements.parent_design_hash).
//
// The algorithm is deliberately fixed: changing it invalidates every stored
// hash at once and would mark every child requirement stale.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
