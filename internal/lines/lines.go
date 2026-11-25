// Package lines reads a file of one record per line without holding more of a
// line than its bound.
package lines

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Each calls fn with each line rd holds, its newline included, and the line's
// number from 1. A line longer than max (the newline aside) is skipped
// without being held in memory; it keeps its number. fn must not keep line.
func Each(rd *bufio.Reader, max int, fn func(n int, line []byte)) error {
	var line []byte
	long := false
	n := 1
	for {
		chunk, err := rd.ReadSlice('\n')
		if !long && len(line)+len(chunk) > max+1 {
			long, line = true, nil
		}
		if !long {
			line = append(line, chunk...)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil && !errors.Is(err, io.EOF):
			return err
		}
		if !long && len(bytes.TrimSuffix(line, []byte("\n"))) <= max && len(line) > 0 {
			fn(n, line)
		}
		if err != nil {
			return nil
		}
		line, long = line[:0], false
		n++
	}
}
