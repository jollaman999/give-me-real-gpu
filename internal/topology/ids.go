package topology

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// stableSum joins the name parts with NUL and returns their sha1.
// Joining with NUL keeps ("ab", "c") and ("a", "bc") from hashing to the same value.
func stableSum(parts []string) [sha1.Size]byte {
	return sha1.Sum([]byte(strings.Join(parts, "\x00")))
}

// StableUUID derives a UUID deterministically from name parts.
// It takes the first 16 bytes of the sha1, sets version nibble 5 and variant 10, and formats them as lower-case 8-4-4-4-12.
// The same names give the same id across restarts, so rows stored in the management platform and the monitoring server are kept.
func StableUUID(parts ...string) string {
	sum := stableSum(parts)
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// StableHex32 derives a 32-character hex string deterministically from name parts (the keystone project and user id format).
func StableHex32(parts ...string) string {
	sum := stableSum(parts)
	return hex.EncodeToString(sum[:16])
}
