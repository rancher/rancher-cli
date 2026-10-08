package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pairwiseAxis is one dimension of a generated sweep: a name for subtest
// names and the values it takes.
type pairwiseAxis struct {
	name   string
	values []string
}

// pairwiseRows returns rows, one value index per axis, in which every pair
// of values of two different axes that some valid row holds appears in at
// least one row.
//
// constrained names the axes valid reads; valid judges a row with every
// constrained axis chosen and reads nothing else. Every row returned is
// valid. The answer depends on nothing but the arguments - no random
// source, no map order - so the rows, and the subtest names made from
// them, are the same on every run and every machine.
//
// The construction is greedy: the first pair no row covers yet is fixed,
// then each remaining axis in order takes the value that covers the most
// uncovered pairs with the axes already chosen, among the values that
// still leave a valid row possible.
func pairwiseRows(axes []pairwiseAxis, constrained []int, valid func(row []int) bool) [][]int {
	possible := newValidTuples(axes, constrained, valid)

	type pair struct{ i, a, j, b int }
	var pairs []pair
	index := map[pair]int{}
	for i := range axes {
		for j := i + 1; j < len(axes); j++ {
			for a := range axes[i].values {
				for b := range axes[j].values {
					partial := unsetRow(len(axes))
					partial[i], partial[j] = a, b
					if possible.extendable(partial) {
						index[pair{i, a, j, b}] = len(pairs)
						pairs = append(pairs, pair{i, a, j, b})
					}
				}
			}
		}
	}
	covered := make([]bool, len(pairs))
	left := len(pairs)
	uncovered := func(i, a, j, b int) bool {
		if i > j {
			i, a, j, b = j, b, i, a
		}
		n, ok := index[pair{i, a, j, b}]
		return ok && !covered[n]
	}

	var rows [][]int
	for next := 0; left > 0; {
		for covered[next] {
			next++
		}
		p := pairs[next]
		row := unsetRow(len(axes))
		row[p.i], row[p.j] = p.a, p.b
		for k := range axes {
			if row[k] >= 0 {
				continue
			}
			best, bestGain := -1, -1
			for v := range axes[k].values {
				row[k] = v
				if !possible.extendable(row) {
					continue
				}
				gain := 0
				for m := range axes {
					if m != k && row[m] >= 0 && uncovered(k, v, m, row[m]) {
						gain++
					}
				}
				if gain > bestGain {
					best, bestGain = v, gain
				}
			}
			row[k] = best
		}
		for i := range axes {
			for j := i + 1; j < len(axes); j++ {
				if n, ok := index[pair{i, row[i], j, row[j]}]; ok && !covered[n] {
					covered[n] = true
					left--
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func unsetRow(n int) []int {
	row := make([]int, n)
	for i := range row {
		row[i] = -1
	}
	return row
}

// validTuples is every valid choice of the constrained axes, indexed by
// axis and value as bit sets, so asking whether a partly chosen row can
// still be completed is an AND of a few sets rather than a search.
type validTuples struct {
	constrained []int
	// sets[c][v] holds the tuples whose c-th constrained axis is v.
	sets  [][][]uint64
	words int
}

func newValidTuples(axes []pairwiseAxis, constrained []int, valid func(row []int) bool) validTuples {
	var tuples [][]int
	row := unsetRow(len(axes))
	var walk func(c int)
	walk = func(c int) {
		if c == len(constrained) {
			if valid(row) {
				t := make([]int, len(constrained))
				for n, k := range constrained {
					t[n] = row[k]
				}
				tuples = append(tuples, t)
			}
			return
		}
		for v := range axes[constrained[c]].values {
			row[constrained[c]] = v
			walk(c + 1)
		}
		row[constrained[c]] = -1
	}
	walk(0)

	vt := validTuples{constrained: constrained, words: (len(tuples) + 63) / 64}
	vt.sets = make([][][]uint64, len(constrained))
	for c, k := range constrained {
		vt.sets[c] = make([][]uint64, len(axes[k].values))
		for v := range vt.sets[c] {
			vt.sets[c][v] = make([]uint64, vt.words)
		}
	}
	for n, t := range tuples {
		for c, v := range t {
			vt.sets[c][v][n/64] |= 1 << (n % 64)
		}
	}
	return vt
}

// extendable reports whether some valid tuple agrees with every
// constrained axis the row has chosen.
func (vt validTuples) extendable(row []int) bool {
	for w := range vt.words {
		word := ^uint64(0)
		for c, k := range vt.constrained {
			if row[k] >= 0 {
				word &= vt.sets[c][row[k]][w]
			}
		}
		if word != 0 {
			return true
		}
	}
	return false
}

// pairwiseRowName is a row's stable subtest name: every axis and its value.
func pairwiseRowName(axes []pairwiseAxis, row []int) string {
	parts := make([]string, len(axes))
	for i, ax := range axes {
		parts[i] = ax.name + "=" + ax.values[row[i]]
	}
	return strings.Join(parts, ",")
}

// TestPairwiseRowsCoverEveryPairOfValues asserts the generator's contract
// on a small constrained space: every row is valid, every pair of values
// some valid row holds is in a row, and two runs give the same rows.
func TestPairwiseRowsCoverEveryPairOfValues(t *testing.T) {
	axes := []pairwiseAxis{
		{"a", []string{"0", "1", "2"}},
		{"b", []string{"0", "1"}},
		{"c", []string{"0", "1", "2", "3"}},
		{"d", []string{"0", "1", "2"}},
	}
	// a=2 never goes with c=3, and b=1 needs d=0.
	valid := func(row []int) bool {
		return (row[0] != 2 || row[2] != 3) && (row[1] != 1 || row[3] == 0)
	}
	constrained := []int{0, 1, 2, 3}

	rows := pairwiseRows(axes, constrained, valid)
	require.Equal(t, rows, pairwiseRows(axes, constrained, valid), "the same rows on every run")

	full := 1
	for _, ax := range axes {
		full *= len(ax.values)
	}
	assert.Less(t, len(rows), full, "fewer rows than the full product")
	for _, row := range rows {
		assert.True(t, valid(row), "row %v", row)
	}
	for i := range axes {
		for j := i + 1; j < len(axes); j++ {
			for a := range axes[i].values {
				for b := range axes[j].values {
					held, inRow := false, false
					for n := range full {
						row := make([]int, len(axes))
						for k, rest := len(axes)-1, n; k >= 0; k-- {
							row[k], rest = rest%len(axes[k].values), rest/len(axes[k].values)
						}
						if row[i] == a && row[j] == b && valid(row) {
							held = true
						}
					}
					for _, row := range rows {
						if row[i] == a && row[j] == b {
							inRow = true
						}
					}
					assert.Equal(t, held, inRow, "%s=%d with %s=%d", axes[i].name, a, axes[j].name, b)
				}
			}
		}
	}
}
