package otherread

import (
	"bytes"
	"io"
)

// Fill calls methods named Read with other signatures.
func Fill(r io.Reader, buf []byte) {
	_, _ = r.Read(buf)
	var b bytes.Buffer
	_, _ = b.Read(buf)
}
