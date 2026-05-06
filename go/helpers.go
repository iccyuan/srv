package main

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
)

func intToStr(i int) string  { return strconv.Itoa(i) }
func uintToStr(i uint32) string { return strconv.FormatUint(uint64(i), 10) }

// randHex4 returns 4 random hex chars (2 bytes). Falls back to a plain
// number on error so we still produce a non-empty id.
func randHex4() string {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b)
}

// strPtr is a tiny helper for json struct fields that need *string.
func strPtr(s string) *string { return &s }

// boolPtr is a tiny helper for json struct fields that need *bool.
func boolPtr(b bool) *bool { return &b }
