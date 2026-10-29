package clierr

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markerRuneSet is the four characters a marker holds after its first one.
const markerRuneSet = "\u2060\u2061\u2062\u2064"

// TestRemedyMarkerIsU2063AndThirtyTwoCharacters asserts the marker opens
// with U+2063, holds 33 characters, takes each of the other 32 from the
// four the mapping uses, and that Rancher is the marker and the word.
func TestRemedyMarkerIsU2063AndThirtyTwoCharacters(t *testing.T) {
	t.Parallel()
	require.Equal(t, 33, utf8.RuneCountInString(RemedyMarker))
	first, size := utf8.DecodeRuneInString(RemedyMarker)
	assert.Equal(t, '\u2063', first)
	for _, r := range RemedyMarker[size:] {
		assert.True(t, strings.ContainsRune(markerRuneSet, r), "U+%04X is not one of the four", r)
	}
	assert.Equal(t, RemedyMarker+"rancher", Rancher)
}

// TestMarkerFromBytesFixesTheMapping asserts the characters one array of
// bytes gives: the bytes in order, the two lowest bits of each byte first,
// 0 for U+2060, 1 for U+2061, 2 for U+2062 and 3 for U+2064.
func TestMarkerFromBytesFixesTheMapping(t *testing.T) {
	t.Parallel()
	got := markerFromBytes([8]byte{0x1B, 0xE4, 0x00, 0xFF, 0x55, 0xAA, 0x0F, 0xF0})
	want := "\u2063" +
		"\u2064\u2062\u2061\u2060" +
		"\u2060\u2061\u2062\u2064" +
		"\u2060\u2060\u2060\u2060" +
		"\u2064\u2064\u2064\u2064" +
		"\u2061\u2061\u2061\u2061" +
		"\u2062\u2062\u2062\u2062" +
		"\u2064\u2064\u2060\u2060" +
		"\u2060\u2060\u2064\u2064"
	assert.Equal(t, want, got)
}

// TestMarkerFromBytesGivesDifferentMarkersForDifferentBytes asserts two
// different arrays of bytes give two different markers.
func TestMarkerFromBytesGivesDifferentMarkersForDifferentBytes(t *testing.T) {
	t.Parallel()
	assert.NotEqual(t, markerFromBytes([8]byte{1}), markerFromBytes([8]byte{2}))
	assert.Equal(t, markerFromBytes([8]byte{1, 2, 3}), markerFromBytes([8]byte{1, 2, 3}))
}

// TestMarkerFromBytesUsesEveryByte asserts each of the 8 positions changes
// the marker: an array that differs from the zero array in one byte gives
// another marker.
func TestMarkerFromBytesUsesEveryByte(t *testing.T) {
	t.Parallel()
	zero := markerFromBytes([8]byte{})
	for i := range 8 {
		var b [8]byte
		b[i] = 1
		assert.NotEqual(t, zero, markerFromBytes(b), "byte %d does not change the marker", i)
	}
}

// TestMarkerFromBytesGivesDifferentMarkersForAThousandInputs asserts a
// thousand different pseudo-random arrays give a thousand different
// markers, which a mapping that drops most bits cannot do. The generator
// has a fixed seed, so the test repeats.
func TestMarkerFromBytesGivesDifferentMarkersForAThousandInputs(t *testing.T) {
	t.Parallel()
	gen := rand.New(rand.NewPCG(1, 2))
	inputs := map[[8]byte]bool{}
	markers := map[string]bool{}
	for len(inputs) < 1000 {
		var b [8]byte
		for i := range b {
			b[i] = byte(gen.Uint32())
		}
		if inputs[b] {
			continue
		}
		inputs[b] = true
		markers[markerFromBytes(b)] = true
	}
	assert.Len(t, markers, 1000)
}

// TestMarkerFromBytesChangesOneCharacterForEachBit asserts that for each of
// the 64 bits, the zero array with only that bit set gives a marker that
// differs from the zero array's marker in exactly one character, so no bit
// is dropped.
func TestMarkerFromBytesChangesOneCharacterForEachBit(t *testing.T) {
	t.Parallel()
	zero := []rune(markerFromBytes([8]byte{}))
	for bit := range 64 {
		var b [8]byte
		b[bit/8] = 1 << (bit % 8)
		got := []rune(markerFromBytes(b))
		require.Len(t, got, len(zero))
		differ := 0
		for i := range got {
			if got[i] != zero[i] {
				differ++
			}
		}
		assert.Equal(t, 1, differ, "bit %d changes %d characters", bit, differ)
	}
}

// TestMarkerBytesFillsEveryPosition asserts that over 100 calls each of the
// 8 positions is non-zero at least once and no two calls return the same
// array.
func TestMarkerBytesFillsEveryPosition(t *testing.T) {
	t.Parallel()
	var seen [8]bool
	arrays := map[[8]byte]bool{}
	for range 100 {
		b := markerBytes()
		arrays[b] = true
		for i, v := range b {
			seen[i] = seen[i] || v != 0
		}
	}
	for i, ok := range seen {
		assert.True(t, ok, "position %d is always zero", i)
	}
	assert.Len(t, arrays, 100)
}

// TestMarkerBytesBitsAreIndependent asserts that over 1000 calls, for every
// pair of the 64 bit positions, all four combinations of the two bits occur at
// least once: no bit is fixed or copied from another, as it would be in bytes
// that repeat a shorter random value.
func TestMarkerBytesBitsAreIndependent(t *testing.T) {
	t.Parallel()
	var seen [64][64]uint8
	for range 1000 {
		b := markerBytes()
		var bits [64]uint8
		for i := range bits {
			bits[i] = b[i/8] >> (i % 8) & 1
		}
		for i := range bits {
			for j := i + 1; j < 64; j++ {
				seen[i][j] |= 1 << (bits[i]<<1 | bits[j])
			}
		}
	}
	for i := range 64 {
		for j := i + 1; j < 64; j++ {
			assert.Equal(t, uint8(0b1111), seen[i][j], "bits %d and %d do not take all four combinations", i, j)
		}
	}
}

// TestExpandRemediesIgnoresAnyOtherMarker asserts only this process's marker
// opens a remedy: U+2063 followed by the word, and the marker with its first,
// a middle and its last character changed, stay as they are, while Rancher
// gains the prefix.
func TestExpandRemediesIgnoresAnyOtherMarker(t *testing.T) {
	t.Parallel()
	const prefix = " --config 'd'"
	runes := []rune(RemedyMarker)
	changed := func(i int) string {
		other := slices.Clone(runes)
		other[i] = '\u2060'
		if other[i] == runes[i] {
			other[i] = '\u2061'
		}
		return string(other)
	}
	for name, marker := range map[string]string{
		"U+2063 alone":     "\u2063",
		"a changed first":  changed(0),
		"a changed middle": changed(len(runes) / 2),
		"a changed last":   changed(len(runes) - 1),
	} {
		text := "run " + marker + "rancher delete x"
		assert.Equal(t, text, ExpandRemedies(text, prefix), name)
	}
	assert.Equal(t, "run rancher --config 'd' delete x", ExpandRemedies("run "+Rancher+" delete x", prefix))
}

// TestShellQuoteRemovesOnlyU2063 asserts a quoted value loses a bare U+2063
// and the first character of the marker, and keeps U+2060, U+2061, U+2062
// and U+2064.
func TestShellQuoteRemovesOnlyU2063(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "'ab'", ShellQuote("a\u2063b"))
	assert.Equal(t, "'a"+RemedyMarker[len("\u2063"):]+"b'", ShellQuote("a"+RemedyMarker+"b"))
	assert.NotContains(t, ShellQuote(RemedyMarker), "\u2063")
	assert.Equal(t, "'a"+markerRuneSet+"b'", ShellQuote("a"+markerRuneSet+"b"))
}

const (
	markerChildEnv    = "CLIERR_MARKER_CHILD"
	markerChildPrefix = "remedy-marker-hex="
)

// TestRemedyMarkerDiffersBetweenProcesses asserts two runs of this test
// binary hold two different markers of 33 characters each. The test runs
// the binary twice as a child process; a child, started with the
// environment switch, prints its marker hex-encoded on one line and ends.
func TestRemedyMarkerDiffersBetweenProcesses(t *testing.T) {
	if os.Getenv(markerChildEnv) != "" {
		fmt.Println(markerChildPrefix + hex.EncodeToString([]byte(RemedyMarker)))
		return
	}
	run := func() string {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRemedyMarkerDiffersBetweenProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), markerChildEnv+"=1")
		out, err := cmd.Output()
		require.NoError(t, err)
		var found []string
		for line := range bytes.Lines(out) {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(string(line)), markerChildPrefix); ok {
				found = append(found, rest)
			}
		}
		require.Len(t, found, 1, "the child printed %q", out)
		raw, err := hex.DecodeString(found[0])
		require.NoError(t, err)
		return string(raw)
	}
	first, second := run(), run()
	assert.NotEqual(t, first, second)
	assert.Equal(t, 33, utf8.RuneCountInString(first))
	assert.Equal(t, 33, utf8.RuneCountInString(second))
}

// BenchmarkMarkerInit is the whole of what a process pays for the marker:
// the random bytes and the mapping.
func BenchmarkMarkerInit(b *testing.B) {
	for b.Loop() {
		_ = markerFromBytes(markerBytes())
	}
}

// BenchmarkMarkerFromBytes is the mapping alone.
func BenchmarkMarkerFromBytes(b *testing.B) {
	input := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	for b.Loop() {
		_ = markerFromBytes(input)
	}
}
