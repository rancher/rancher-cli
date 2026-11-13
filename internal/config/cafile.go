package config

import "github.com/rancher/rancher-cli/internal/clierr"

// CAFileMeaning is the clause that says what <file> stands for in a message
// that names a CA flag with a file placeholder (see CAFileArg). A message
// holds it once, before the first command that holds <file>, or after the
// clause that names <file> when no command holds it.
const CAFileMeaning = "<file> is a file that holds the server's CA certificate"

// CAFileFlag is the flag text of a command that takes the CA from a file.
const CAFileFlag clierr.CommandWord = " --cacert <file>"

// CAFileArg is the CA flag, --cacert or the legacy command's --cacerts, with
// the <file> placeholder after it.
func CAFileArg(flag string) string {
	return flag + " <file>"
}
