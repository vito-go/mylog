package mylog

import (
	"crypto/rand"
	"time"

	"github.com/google/uuid"
)

// UUID returns a version 7 UUID. If the standard generation fails,
// it falls back to a custom implementation.
func UUID() uuid.UUID {
	return genUUID()
}

func genUUID() uuid.UUID {
	// Attempt to generate a cryptographically secure UUID v7
	if v7UUID, err := uuid.NewV7(); err == nil {
		return v7UUID
	}
	// Fallback if the system's entropy source is unavailable
	return generateFallbackUUIDv7()
}

// generateFallbackUUIDv7 generates a local fallback UUID v7.
// It is compliant with RFC 9562 and maintains time-based sortability,
// but it is NOT cryptographically secure.
func generateFallbackUUIDv7() uuid.UUID {
	// 48-bit millisecond timestamp (Big-Endian)
	nowMs := uint64(time.Now().UnixMilli())

	var u uuid.UUID

	// UUID v7 Layout (RFC 9562):
	// - Bytes[0:6]:  48-bit timestamp
	// - Bytes[6]:     4-bit version (0111) + 4 bits random
	// - Bytes[7]:     8 bits random
	// - Bytes[8]:     2-bit variant (10) + 6 bits random
	// - Bytes[9:16]: 56 bits random

	// Fill timestamp (First 6 bytes)
	u[0] = byte(nowMs >> 40)
	u[1] = byte(nowMs >> 32)
	u[2] = byte(nowMs >> 24)
	u[3] = byte(nowMs >> 16)
	u[4] = byte(nowMs >> 8)
	u[5] = byte(nowMs)

	rand.Read(u[6:])

	// Set version = 7 (High 4 bits of Byte 6)
	u[6] = (u[6] & 0x0f) | 0x70

	// Set variant = 10 (High 2 bits of Byte 8)
	u[8] = (u[8] & 0x3f) | 0x80

	return u
}
