package engine

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type matchFixture struct {
	ids     []string
	display map[string]string
	cands   []clierr.Candidate
}

func newMatchFixture(pairs ...string) matchFixture {
	f := matchFixture{display: map[string]string{}}
	for i := 0; i < len(pairs); i += 2 {
		f.ids = append(f.ids, pairs[i])
		f.display[pairs[i]] = pairs[i+1]
		f.cands = append(f.cands, clierr.Candidate{ID: pairs[i], Name: pairs[i+1]})
	}
	return f
}

func (f matchFixture) match(v string, t MatchText) (string, string, error) {
	return MatchOne(v, f.ids, func(id string) string { return f.display[id] }, f.cands, t)
}

var projectText = MatchText{Noun: "project", Plural: "projects", Flag: "--project",
	NotFound: "project not found", NotFoundHint: "list the projects"}

// TestMatchOneIDMatchWinsWithoutAWarningWhenNoDisplayNameMatches asserts an
// id match resolves silently when no other object has v as its display
// name.
func TestMatchOneIDMatchWinsWithoutAWarningWhenNoDisplayNameMatches(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-1", "web", "p-2", "db")
	id, warning, err := f.match("p-1", projectText)
	require.NoError(t, err)
	assert.Equal(t, "p-1", id)
	assert.Empty(t, warning)
}

// TestMatchOneIDMatchWinsOverDisplayNameMatches asserts the id wins whether
// one or several other objects have v as their display name, with the
// warning wording for the flag form and the positional form.
func TestMatchOneIDMatchWinsOverDisplayNameMatches(t *testing.T) {
	t.Parallel()
	positional := projectText
	positional.Flag = ""
	one := newMatchFixture("p-1", "first", "p-2", "p-1")
	several := newMatchFixture("p-3", "p-1", "p-1", "first", "p-2", "p-1")
	for name, tc := range map[string]struct {
		f    matchFixture
		t    MatchText
		want string
	}{
		"one other, flag":          {one, projectText, "--project 'p-1' is the id of project 'p-1'; project 'p-2' also has the display name 'p-1' - pass --project 'p-2' for that one"},
		"one other, positional":    {one, positional, "'p-1' is the id of project 'p-1'; project 'p-2' also has the display name 'p-1' - pass 'p-2' for that one"},
		"several others, flag":     {several, projectText, "--project 'p-1' is the id of project 'p-1'; projects 'p-2', 'p-3' also have the display name 'p-1' - pass --project with the id of the one you mean"},
		"several others, position": {several, positional, "'p-1' is the id of project 'p-1'; projects 'p-2', 'p-3' also have the display name 'p-1' - pass the id of the one you mean"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			id, warning, err := tc.f.match("p-1", tc.t)
			require.NoError(t, err)
			assert.Equal(t, "p-1", id)
			assert.Equal(t, tc.want, warning)
		})
	}
}

func TestMatchOneOneDisplayNameMatchResolves(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-1", "web", "p-2", "db")
	id, warning, err := f.match("db", projectText)
	require.NoError(t, err)
	assert.Equal(t, "p-2", id)
	assert.Empty(t, warning)
}

// TestMatchOneDuplicateDisplayNamesAreAmbiguousAndSortedByID asserts two or
// more display-name matches with no id match are AMBIGUOUS_SELECTION whose
// candidates are only the matches, sorted by id, whatever order they were
// given in.
func TestMatchOneDuplicateDisplayNamesAreAmbiguousAndSortedByID(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-3", "web", "p-9", "db", "p-1", "web", "p-2", "web")
	_, _, err := f.match("web", projectText)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "3 projects have display name 'web'; pass the project id", ce.Message)
	assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "web"}, {ID: "p-2", Name: "web"}, {ID: "p-3", Name: "web"}}, ce.Candidates)
}

// TestMatchOneNotFoundCarriesTheTextAndEveryCandidate asserts no match is
// NOT_FOUND with the caller's message and hint and every candidate in the
// order given.
func TestMatchOneNotFoundCarriesTheTextAndEveryCandidate(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-2", "db", "p-1", "api")
	_, _, err := f.match("web", projectText)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found", ce.Message)
	assert.Equal(t, "list the projects", ce.Hint)
	assert.Equal(t, f.cands, ce.Candidates)
}
