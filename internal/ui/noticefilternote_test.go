package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

const noticeNote = "characters a terminal cannot show were left out or replaced\n"

// TestNoticeSaysWhenAFilterChangedIt asserts a notice is followed by the
// note line once when a builder's filter changed a value in it (the mark),
// when its own filter changes it, and when the rewrite cuts the clause that
// had the mark, and never for a notice whose only remedy command lost its
// marker.
func TestNoticeSaysWhenAFilterChangedIt(t *testing.T) {
	t.Parallel()
	cut := func(s string) string { before, _, _ := strings.Cut(s, "; "); return before }
	for _, tc := range []struct {
		name, msg, want string
		rewrite         func(string) string
	}{
		{name: "a value a builder changed", msg: "name ab" + clierr.RemovalMark, want: "name ab\n" + noticeNote},
		{name: "a value the notice filter changes", msg: "name a\u200bb", want: "name ab\n" + noticeNote},
		{name: "the rewrite cuts the clause with the mark", msg: "name; x" + clierr.RemovalMark, rewrite: cut, want: "name\n" + noticeNote},
		{name: "a remedy command", msg: "run " + clierr.Rancher + " auth status",
			rewrite: func(s string) string { return clierr.ExpandRemedies(s, "") }, want: "run rancher auth status\n"},
		{name: "nothing to remove", msg: "name ab", want: "name ab\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			p := New(&errBuf, strings.NewReader(""), true, false)
			if tc.rewrite != nil {
				p = p.WithNoticeRewrite(tc.rewrite)
			}
			p.Notice(tc.msg)
			assert.Equal(t, tc.want, errBuf.String())
		})
	}
}
