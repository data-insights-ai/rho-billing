// Package identity encodes persisted operation fingerprints. It is not a host API.
package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// Fingerprint hashes length-delimited fields so opaque IDs cannot collide by
// introducing separators. Callers supply canonical field order and encoding.
func Fingerprint(fields ...string) string {
	h := sha256.New()
	for _, field := range fields {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		h.Write(size[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Instant preserves the existing UTC microsecond identity encoding.
func Instant(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) }
