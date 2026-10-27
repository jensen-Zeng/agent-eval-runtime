package jobs

import "strings"

// ValidIdempotencyKey matches the public API constraint. An empty key means
// that deduplication is not requested; batch manifests require a nonempty key.
func ValidIdempotencyKey(key string) bool {
	if len(key) > 128 || strings.TrimSpace(key) != key {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7e {
			return false
		}
	}
	return true
}
