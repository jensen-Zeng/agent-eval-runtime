package scheduler

import (
	"bufio"
	"errors"
	"fmt"
)

func readTraceLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > maxTraceLine+1 {
			return nil, fmt.Errorf("trace event exceeds line limit")
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}
