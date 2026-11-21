package engine

import "bytes"

// DropLeadingByteOrderMarks returns b without the byte order marks (U+FEFF,
// the bytes EF BB BF) at its start. Windows PowerShell 5.1 sends one mark
// before piped text, and two when its $OutputEncoding is
// `[System.Text.Encoding]::UTF8`. A JSON reader accepts none, and a YAML
// reader that meets a second mark loses the first character of every
// top-level key after the first. A mark anywhere else is part of the value.
// UTF-16 (the bytes FF FE) is not handled.
//
// The result is nil when every byte is a mark and a sub-slice of b otherwise;
// the input is not changed. Callers treat nil and empty alike.
func DropLeadingByteOrderMarks(b []byte) []byte {
	return bytes.TrimLeft(b, "\ufeff")
}
