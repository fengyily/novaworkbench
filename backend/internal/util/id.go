package util

import (
	"crypto/rand"
	"fmt"
)

func NewID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%s_%x", prefix, b)
}

// NewUUID returns a random RFC 4122 version 4 UUID string (8-4-4-4-12).
// Used for the claude CLI --session-id flag, which requires a valid UUID.
func NewUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	// version 4
	b[6] = (b[6] & 0x0f) | 0x40
	// variant 10xxxxxx
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
