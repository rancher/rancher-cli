// Package fsacl sets owner-only access on files and directories where the
// operating system ignores Unix mode bits. On Windows an NTFS access list
// replaces the 0600 and 0700 modes; on every other platform the calls do
// nothing, because the mode bits the caller already set are the
// restriction. The package imports only the standard library and
// golang.org/x/sys/windows; any package may use it without taking on
// configuration, credential or network code.
package fsacl
