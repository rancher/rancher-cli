package rootwriter

import (
	"io"

	"github.com/urfave/cli/v3"
)

// Other has a field named Writer that is not the field of the urfave Command.
type Other struct{ Writer io.Writer }

func read(cmd *cli.Command) io.Writer { return cmd.Writer }

func assign(cmd *cli.Command, w io.Writer) { cmd.Writer = w }

func literalKey(w io.Writer) *cli.Command { return &cli.Command{Writer: w} }

func root(cmd *cli.Command) io.Writer { return cmd.Root().Writer }

func otherType(o Other) io.Writer { return o.Writer }

func otherLiteral(w io.Writer) Other { return Other{Writer: w} }
