// Package secret defines the type of the secret values the CLI reads from a
// flag, a file or stdin. A Value prints <hidden> in every format and encoding;
// only Reveal returns its text, and a test lists every call site of Reveal
// with its reason. The package imports only the standard library.
package secret
