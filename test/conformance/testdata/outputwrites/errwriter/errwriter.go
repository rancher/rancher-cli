package errwriter

import (
	"io"

	"github.com/rancher/rancher-cli/internal/cli"
)

// Other has a field named ErrWriter that is not the field of the Command of internal/cli.
type Other struct{ ErrWriter io.Writer }

func read(cmd *cli.Command) io.Writer { return cmd.ErrWriter }

func assign(cmd *cli.Command, w io.Writer) { cmd.ErrWriter = w }

func literalKey(w io.Writer) *cli.Command { return &cli.Command{ErrWriter: w} }

func root(cmd *cli.Command) io.Writer { return cmd.Root().ErrWriter }

func otherType(o Other) io.Writer { return o.ErrWriter }

func otherLiteral(w io.Writer) Other { return Other{ErrWriter: w} }
