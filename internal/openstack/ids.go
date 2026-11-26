package openstack

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// stableUUID derives a lowercase 8-4-4-4-12 id from parts. The same parts
// always give the same id, so restarts keep every id the client has stored.
func stableUUID(parts ...string) string {
	sum := sha1.Sum([]byte("give-me-real-gpu/openstack\x00" + strings.Join(parts, "\x00")))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// stableHex32 derives a 32 character hex id (keystone id format) from parts.
func stableHex32(parts ...string) string {
	return strings.ReplaceAll(stableUUID(parts...), "-", "")
}

// stableUint derives a number from parts, used to spread fake values.
func stableUint(parts ...string) uint64 {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return binary.BigEndian.Uint64(sum[:8])
}

// macFor returns a neutron style MAC address (fa:16:3e prefix) for a port.
func macFor(id string) string {
	sum := sha1.Sum([]byte("mac\x00" + id))
	return fmt.Sprintf("fa:16:3e:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// hostIDFor mimics nova hostId: a 56 character hash of project and host.
func hostIDFor(projectID, host string) string {
	sum := sha256.Sum224([]byte(projectID + host))
	return hex.EncodeToString(sum[:])
}

func itoa(i int) string { return strconv.Itoa(i) }

// createdAt is a stable creation time for a resource, spread over the 30
// days before 2026-11-01.
func createdAt(id string) time.Time {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(stableUint("created", id)%(30*86400)) * time.Second)
}

// Time layouts used by the real services.
const (
	layoutISOZ      = "2006-01-02T15:04:05Z"        // nova created/updated, neutron
	layoutMicro     = "2006-01-02T15:04:05.000000"  // nova launched_at, os-services updated_at
	layoutMicroZ    = "2006-01-02T15:04:05.000000Z" // keystone issued_at/expires_at
	layoutAgentTime = "2006-01-02 15:04:05"         // neutron agent timestamps
)

func fmtTime(t time.Time, layout string) string { return t.UTC().Format(layout) }
