package engine

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Non-TTY Docker logs and exec multiplex stdout/stderr in eight-byte frames.
func copyDockerOutput(dst io.Writer, src io.Reader) error {
	var header [8]byte
	for {
		n, err := io.ReadFull(src, header[:])
		if err == io.EOF && n == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if header[0] > 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return fmt.Errorf("invalid Docker stream frame")
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > 16<<20 {
			return fmt.Errorf("Docker stream frame exceeds limit")
		}
		if _, err = io.CopyN(dst, src, int64(size)); err != nil {
			return err
		}
	}
}
