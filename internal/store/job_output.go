package store

import (
	"strings"
	"unicode/utf8"
)

const maxJobOutputBytes = 64 << 10

// boundedJobOutput keeps the recent log tail valid for utf8mb4 persistence.
// Repair first, since replacement runes can increase the byte count, then
// advance the tail boundary past any partial UTF-8 character.
func boundedJobOutput(output string) string {
	output = strings.ToValidUTF8(output, "\uFFFD")
	if len(output) <= maxJobOutputBytes {
		return output
	}
	start := len(output) - maxJobOutputBytes
	for start < len(output) && !utf8.RuneStart(output[start]) {
		start++
	}
	return output[start:]
}
