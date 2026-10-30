package cli

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// lineRefusal returns the refusal of the line p holds, or nil to let urfave
// run it. wholeLine is true when the refusal renders in the output format
// and color the whole typed line selects, the words after a "-" included.
// The others render in the ones the parsed line selects when the walk read
// the whole line (parsedLine.readWholeLine), and in the ones the line scan
// finds otherwise. Three refusals are tried in order:
//   - words typed after a "-" on its own;
//   - a bool flag typed with "=" and an empty value, or one of only
//     whitespace, which urfave parses as true or rejects;
//   - a positional word that reads as a bool, typed as its own word after
//     a bool flag, which takes its value only joined with "=".
//
// A line is judged as urfave reads it up to the word a refusal is about: a
// help display or version display urfave reaches before that word, and an
// error it reports before it, win over the refusal. A bool flag typed with
// "=" and a value of only whitespace is an error urfave reports, and the
// line is read on past it, so a "-" on its own after it is refused as one.
func lineRefusal(p parsedLine) (ce *clierr.Error, wholeLine bool) {
	if words, level, ok := wordsAfterDash(p); ok {
		if level.leaf {
			if d, found := commandDescriptor(level.cmd); found && !d.SecretInput {
				return runtime.DroppedWordsError(words), true
			}
		}
		return runtime.DroppedWordsCountError(len(words)), true
	}
	reached := p.endIfBlankAccepted.kind == lineRuns && p.levels[p.endIfBlankAccepted.level].leaf
	if flags := emptyBoolSpellings(p, reached); len(flags) > 0 {
		return runtime.EmptyBoolError(flags), false
	}
	if reached {
		return boolValueRefusal(p), false
	}
	return nil, false
}

// wordsAfterDash returns the words, as typed, after the first "-" on its
// own the walk reads, when it has any to refuse. At a leaf the help flag
// holding true before the "-" shows help, and at the root the version flag
// set before it, with the help flag not true, prints the version. At the
// root or a group the "-" is the word urfave takes for the command name;
// one that comes after another argument is not read. It also returns the
// level the "-" is typed at, which is a leaf when the walk has reached the
// command.
func wordsAfterDash(p parsedLine) ([]string, *lineLevel, bool) {
	for _, lv := range p.levels {
		for _, w := range lv.words {
			if !w.dash {
				continue
			}
			switch {
			case w.index == len(p.argv)-1:
				return nil, nil, false
			case lv.leaf && w.helpOn:
				return nil, nil, false
			case lv.version != nil && w.versionOn && !w.helpOn:
				return nil, nil, false
			case !lv.leaf && (len(lv.argAt) == 0 || lv.argAt[0] != w.index):
				return nil, nil, false
			}
			return p.argv[w.index+1:], lv, true
		}
	}
	return nil, nil, false
}

// emptyBoolSpellings returns the bool flags the walk reads typed with "=" and
// an empty value or one of only whitespace, each as typed without the
// "=". A value of only whitespace counts when the level's help flag did not
// hold true before the word. An empty value counts when the walk reaches a
// leaf, the help and version flags left out: urfave reads it as true.
func emptyBoolSpellings(p parsedLine, reachedLeaf bool) []string {
	var flags []string
	for _, lv := range p.levels {
		for _, w := range lv.words {
			if w.flag == nil || !w.flag.isBool {
				continue
			}
			blank := w.blank && !w.helpOn
			empty := w.empty && reachedLeaf && !w.flag.builtin
			if !blank && !empty {
				continue
			}
			spelling, _, _ := strings.Cut(w.text, "=")
			flags = append(flags, spelling)
		}
	}
	return flags
}

// boolValueRefusal returns the refusal of a positional word of the leaf the
// walk reaches that reads as a bool and follows a bool flag typed without a
// value, or nil.
func boolValueRefusal(p parsedLine) *clierr.Error {
	leaf := p.levels[p.endIfBlankAccepted.level]
	d, ok := commandDescriptor(leaf.cmd)
	if !ok {
		return clierr.New(clierr.CodeGeneric, "internal error: "+p.commandName(p.endIfBlankAccepted.level)+" has no command definition")
	}
	byIndex := map[int]lineWord{}
	inLeaf := map[int]bool{}
	for _, lv := range p.levels {
		for _, w := range lv.words {
			byIndex[w.index] = w
			inLeaf[w.index] = lv == leaf
		}
	}
	var vals []runtime.BoolValue
	var args []string
	skipped := map[int]bool{}
	joined := map[int]string{}
	for i, w := range leaf.words {
		if w.kind != plainWord {
			continue
		}
		args = append(args, w.text)
		if isBoolValueWord(leaf.words, i) {
			vals = append(vals, runtime.BoolValue{Index: len(args) - 1, Flag: leaf.words[i-1].text})
			skipped[w.index] = true
			joined[leaf.words[i-1].index] = "=" + clierr.ShellQuote(strings.TrimSpace(w.text))
		}
	}
	if len(vals) == 0 {
		return nil
	}
	return runtime.BoolValueError(d, args, vals, hintLine(p.argv, byIndex, inLeaf, skipped, joined))
}

// hintLine writes the command line typed, each word as typed, with each
// bool value joined to its flag, so the line runs as written: command names
// bare, the words of the leaf and flag values quoted. A word no level read
// is the command name typed after a "--", which a group hands on: every
// other word of a line that reaches a leaf is read by a level.
func hintLine(argv []string, byIndex map[int]lineWord, inLeaf, skipped map[int]bool, joined map[int]string) string {
	parts := []string{"rancher"}
	for i, word := range argv {
		switch w, read := byIndex[i]; {
		case skipped[i]:
		case read:
			parts = append(parts, quoteWord(w.tailWord, w.kind == plainWord && !inLeaf[i])+joined[i])
		default:
			parts = append(parts, word)
		}
	}
	return strings.Join(parts, " ")
}

// isBoolValueWord reports whether words[i] is a positional word that reads
// as a bool, typed as its own word directly after a bool flag typed without
// a joined value.
func isBoolValueWord(words []lineWord, i int) bool {
	return i > 0 && !words[i].unread && isBoolValueAfter(words[i-1].tailWord, words[i].tailWord)
}

// quoteWord writes w for a command line: a flag as typed with a joined
// value quoted, "--" as typed, a word bare when bare is set (a command
// name) and quoted otherwise, like a flag's value.
func quoteWord(w tailWord, bare bool) string {
	switch {
	case w.kind == flagWord && w.joined:
		name, value, _ := strings.Cut(w.text, "=")
		return name + "=" + clierr.ShellQuote(value)
	case w.kind == flagWord, w.kind == endWord, bare:
		return w.text
	default:
		return clierr.ShellQuote(w.text)
	}
}
