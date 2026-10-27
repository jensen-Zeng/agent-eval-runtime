package parser

import (
	"errors"
	"strconv"
)

var ErrEmpty = errors.New("empty count")
var ErrNegative = errors.New("count must be non-negative")

// ParseCount parses a non-negative base-10 count. Surrounding whitespace is allowed.
func ParseCount(input string) (int, error) {
	if input == "" {
		return 0, ErrEmpty
	}
	value, err := strconv.Atoi(input)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, ErrNegative
	}
	return value, nil
}