package rangeclamp

import "errors"

var ErrRange = errors.New("minimum exceeds maximum")

// Clamp is the intentionally buggy baseline: it returns value without clamping.
func Clamp(value, min, max int) (int, error) {
	return value, nil
}
