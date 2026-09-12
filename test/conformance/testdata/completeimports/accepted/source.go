// A complete package that stays inside the allowlist: the seam, the kind
// table, structured errors, and the permitted standard-library packages.
package complete

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{context.Background, errors.New, fmt.Sprint, maps.Keys[map[string]int], slices.Sort[[]string], sort.Strings, strconv.ParseBool, strings.Cut, clierr.New, kinds.Lookup, completion.NewConnection}
