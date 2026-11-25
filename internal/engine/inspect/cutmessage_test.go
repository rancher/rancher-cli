package inspect

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestACutMessageCountsItsHiddenCharacters asserts that a condition message
// and an agent message over the limit are cut before the filter: a hidden
// character before the cut counts toward the limit, and the printed line
// has the visible characters before the cut, the ellipsis and the removal
// mark, which the filter adds after the cut.
func TestACutMessageCountsItsHiddenCharacters(t *testing.T) {
	t.Parallel()
	msg := strings.Repeat("a", 10) + "\u200b" + strings.Repeat("b", 189)
	printed := strings.Repeat("a", 10) + strings.Repeat("b", 109) + "\u2026" + clierr.RemovalMark
	disconnected := false
	r := clusterResult{
		Cluster: clusterIdentity{ID: "c-x"},
		Agent:   &agentStatus{Connected: &disconnected, Message: msg},
		Status: &clusterStatus{Conditions: []clusterCondition{
			{Type: "Ready", Status: "False", Message: msg},
		}},
	}
	text := renderClusterText(r).Marked()
	assert.Contains(t, text, "\n  agent: not connected - "+printed+"\n", "the agent line")
	assert.Contains(t, text, "\n  Ready=False "+printed+"\n", "the condition line")
	assert.NotContains(t, text, strings.Repeat("b", 110), "the hidden character counts toward the limit")

	short := strings.Repeat("\u200b", 10) + strings.Repeat("c", 115)
	r.Status.Conditions[0].Message = short
	assert.Contains(t, renderClusterText(r).Marked(), "\n  Ready=False "+strings.Repeat("c", 110)+"\u2026"+clierr.RemovalMark+"\n", "125 characters, 115 of them visible, are cut at 120")
}

// TestACutMessageMarksAnInvalidByteBeforeTheCut asserts that a condition
// message and an agent message over the limit, with an invalid byte before
// the cut and no hidden character, are cut with the byte as it came: the
// filter after the cut writes it as U+FFFD and adds the removal mark.
func TestACutMessageMarksAnInvalidByteBeforeTheCut(t *testing.T) {
	t.Parallel()
	msg := strings.Repeat("a", 20) + "\xff" + strings.Repeat("b", 179)
	printed := strings.Repeat("a", 20) + "\ufffd" + strings.Repeat("b", 99) + "\u2026" + clierr.RemovalMark
	disconnected := false
	r := clusterResult{
		Cluster: clusterIdentity{ID: "c-x"},
		Agent:   &agentStatus{Connected: &disconnected, Message: msg},
		Status: &clusterStatus{Conditions: []clusterCondition{
			{Type: "Ready", Status: "False", Message: msg},
		}},
	}
	text := renderClusterText(r).Marked()
	assert.Contains(t, text, "\n  agent: not connected - "+printed+"\n", "the agent line")
	assert.Contains(t, text, "\n  Ready=False "+printed+"\n", "the condition line")
}

// TestTruncateRunesKeepsTheBytesBeforeTheCut asserts that truncateRunes
// counts characters of the string as it is, an invalid byte as one, and
// returns the bytes before the cut unchanged, followed by the ellipsis.
func TestTruncateRunesKeepsTheBytesBeforeTheCut(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a\xffb\u2026", truncateRunes("a\xffbcd", 3))
	assert.Equal(t, "a\xffb", truncateRunes("a\xffb", 3), "a string of exactly limit characters is returned as it is")
	assert.Equal(t, "\u00e9\u4e16\u2026", truncateRunes("\u00e9\u4e16x", 2), "a rune of several bytes counts once")

	long := strings.Repeat("a", 20) + "\xff" + strings.Repeat("b", 179)
	assert.Equal(t, strings.Repeat("a", 20)+"\xff"+strings.Repeat("b", 99)+"\u2026", truncateRunes(long, conditionMessageLimit))
}
