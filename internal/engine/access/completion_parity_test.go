package access

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/access/complete"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompletionWalksTheSamePagesAsTheCommand asserts the --cluster
// completion source of `access projects` and the command's own one-pass
// reader, fed the same steve pages, answer the same cluster set: the same
// ids, the same display names, the same order, the management cluster
// row inserted alike, and the same request sequence.
func TestCompletionWalksTheSamePagesAsTheCommand(t *testing.T) {
	t.Parallel()
	pages := map[string]stevePage{
		"":       {Data: []map[string]any{steveItem("c-m-b", "b")}, Count: new(3), Continue: "2 of 3", Revision: "9"},
		"2 of 3": {Data: []map[string]any{steveItem("c-m-a", "a")}, Count: new(3), Continue: "x&y", Revision: "9"},
		"x&y":    {Data: []map[string]any{steveItem("c-m-c", "c")}, Count: new(3), Revision: "9"},
	}
	srv, paths := pagedServer(t, pages)
	rows, err := ListClustersOnce(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	commandPaths := append([]string(nil), *paths...)
	*paths = nil

	conn := completion.NewConnection(srv.Client(), "tok", srv.URL, false, steve.MaxPages)
	got := complete.ProjectsClusterSource().Complete(t.Context(), conn, completion.Request{Flag: "cluster"})

	var want []string
	for _, r := range rows {
		want = append(want, r.ID)
		if r.DisplayName != r.ID {
			want = append(want, r.DisplayName)
		}
	}
	assert.Equal(t, want, got.Names)
	assert.Equal(t, commandPaths, *paths)
	assert.Equal(t, len(commandPaths), conn.Used())
}

// TestCompletionRefusesWhatTheCommandRefuses asserts a moved revision
// fails the command's one-pass reader and empties the completion source
// alike.
func TestCompletionRefusesWhatTheCommandRefuses(t *testing.T) {
	t.Parallel()
	srv, _ := pagedServer(t, map[string]stevePage{
		"":  {Data: []map[string]any{steveItem("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
		"n": {Data: []map[string]any{steveItem("c-m-b", "b")}, Count: new(2), Revision: "2"},
	})
	_, err := ListClustersOnce(t.Context(), fakeRootClient{srv})
	require.Error(t, err)
	got := complete.ProjectsClusterSource().Complete(t.Context(), completion.NewConnection(srv.Client(), "tok", srv.URL, false, steve.MaxPages), completion.Request{Flag: "cluster"})
	assert.Empty(t, got.Names)
}
