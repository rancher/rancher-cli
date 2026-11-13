package collected

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func builderWriteString(name string) string {
	var b strings.Builder
	b.WriteString(clierr.ShellQuote(name))
	return strconv.Quote(b.String())
}

func bufferFprintf(name string) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s", clierr.ShellQuote(name))
	return strconv.Quote(buf.String())
}

func bufferWrite() string {
	var buf bytes.Buffer
	buf.Write([]byte(clierr.Rancher))
	return strconv.Quote(buf.String())
}

func sliceIndex() string {
	parts := make([]string, 2)
	parts[1] = clierr.Rancher
	return strconv.Quote(strings.Join(parts, " "))
}

func sliceAppend() string {
	var parts []string
	parts = append(parts, "get", clierr.Rancher)
	return strconv.Quote(strings.Join(parts, " "))
}

func sliceLiteral() string {
	return strconv.Quote(strings.Join([]string{clierr.Rancher}, " "))
}
