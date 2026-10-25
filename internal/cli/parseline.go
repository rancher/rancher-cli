package cli

import (
	"slices"
	"strconv"
	"strings"

	ucli "github.com/urfave/cli/v3"
)

// lineFlag is one flag a level of a command line parses, with what the walk
// has read for it so far.
type lineFlag struct {
	names  []string
	isBool bool
	// once is set for a flag urfave refuses to read a second time, and
	// builtin for the help and version flags urfave adds.
	once, builtin bool
	// count is how many times the flag was set, a value urfave rejects
	// included. set is true once a value urfave accepts was read, value is
	// a bool flag's value after the last such one, and text a value flag's.
	count int
	set   bool
	value bool
	text  string
}

// lineWord is one word a level reads, with the state of the level's help and
// version flags before it.
type lineWord struct {
	tailWord
	// index is the word's place in the line.
	index int
	// flag is the flag a defined flag word names.
	flag *lineFlag
	// helpOn is true when the level's help flag held true before this
	// word, versionOn when the version flag was set before it.
	helpOn, versionOn bool
	// blank and empty mark a bool flag word typed with "=" and a value that
	// is only whitespace, or nothing at all.
	blank, empty bool
	// unread marks a word the level takes as an argument without reading
	// it as a flag or a command name.
	unread bool
}

// lineLevel is one command the walk of a line visits.
type lineLevel struct {
	cmd *ucli.Command
	// flags are the flags the level parses: the first own of them are the
	// command's own, then the ones its ancestors apply to it.
	flags []*lineFlag
	own   int
	// help and version are the level's help flag and, at the root, version
	// flag; nil where urfave adds none.
	help, version *lineFlag
	// words are the words the level reads, in order, and args the words it
	// takes as arguments, as typed, with their places in the line.
	words []lineWord
	args  []string
	argAt []int
	// leaf is true for a command with no subcommands.
	leaf bool
}

// lineEndKind says how urfave's pass over a line ends.
type lineEndKind int

const (
	// lineRuns: the command of the last level runs its action. A root or a
	// group with an argument left reports an unknown command; one without
	// shows its help.
	lineRuns lineEndKind = iota
	// lineHelp: the last level shows help, as its help flag asks, or after
	// a parse error when the flag was on before it.
	lineHelp
	// lineVersion: the root prints the version.
	lineVersion
	// lineError: urfave reports a parse error at the last level.
	lineError
)

// lineErrorClass is the kind of parse error urfave reports.
type lineErrorClass int

const (
	undefinedFlag lineErrorClass = iota + 1
	missingFlagValue
	repeatedFlag
	badBoolValue
)

// lineEnd is where urfave's pass over a line ends.
type lineEnd struct {
	kind lineEndKind
	// level is the index of the last level visited.
	level int
	// afterError is true for help shown after a parse error: the refusals
	// then render in the format the whole line selects (readWholeLine).
	// class is the kind of parse error; the parse-match test reads it to
	// compare urfave's end state, and leafOfLine to find an undefined flag.
	afterError bool
	class      lineErrorClass
}

// parsedLine is a command line read the way urfave reads it.
type parsedLine struct {
	argv   []string
	levels []*lineLevel
	// urfaveEnd is where urfave's pass ends; the parse-match test and
	// leafOfLine read it. endIfBlankAccepted is where the pass would end if urfave
	// accepted a bool flag typed with "=" and a value of only whitespace
	// and read on: the same unless the line holds such a word, and the one
	// the refusals read. levels hold the walk to that end.
	urfaveEnd, endIfBlankAccepted lineEnd
}

// inWord is a word handed to a level, with its place in the line.
type inWord struct {
	text  string
	index int
}

// parseLine parses argv, the words typed after the program name, the way
// urfave parses and dispatches them on root's command tree, before root has
// run or while it runs. It does not change the tree: a flag's state is kept
// in the walk.
//
// A tree no run has set up lacks what urfave adds inside its run: the
// parent of each command, the help flag of every command, the version flag
// of the root. The flags each level parses are built from the chain of
// commands the walk went through instead. Inside a run urfave has added its
// help flags to the commands it set up, and the version flag to the root
// only, so builtinFlag finds a flag of that name already there and returns
// nil: the walk then reads --help and -h, and at the root --version and -v,
// as the plain bool flags they are.
func parseLine(root *ucli.Command, argv []string) parsedLine {
	w := &walker{flags: map[ucli.Flag]*lineFlag{}}
	w.p.argv = argv
	in := make([]inWord, len(argv))
	for i, a := range argv {
		in[i] = inWord{a, i}
	}
	chain := []*ucli.Command{root}
	for {
		lv := w.enter(chain)
		pos, class := w.parseLevel(lv, in)
		at := len(w.p.levels) - 1
		for _, p := range pos {
			lv.args = append(lv.args, p.text)
			lv.argAt = append(lv.argAt, p.index)
		}
		switch {
		case class != 0 && lv.help != nil && lv.help.value:
			w.finish(lineEnd{kind: lineHelp, level: at, afterError: true})
		case class != 0:
			w.finish(lineEnd{kind: lineError, level: at, class: class})
		case lv.help != nil && lv.help.value:
			w.finish(lineEnd{kind: lineHelp, level: at})
		case lv.version != nil && lv.version.set:
			w.finish(lineEnd{kind: lineVersion, level: at})
		default:
			if len(pos) > 0 {
				if sub := lv.cmd.Command(pos[0].text); sub != nil {
					chain = append(chain, sub)
					in = pos[1:]
					continue
				}
			}
			w.finish(lineEnd{kind: lineRuns, level: at})
		}
		return w.p
	}
}

// walker holds the state of one pass over a line.
type walker struct {
	p parsedLine
	// flags maps each flag of the tree the walk meets to its state in the
	// walk, so a flag a subcommand inherits is one flag at every level.
	flags map[ucli.Flag]*lineFlag
	// blankSeen is true once the pass met a bool flag typed with "=" and a
	// value of only whitespace.
	blankSeen bool
}

// finish records where the pass ends.
func (w *walker) finish(end lineEnd) {
	w.p.endIfBlankAccepted = end
	if !w.blankSeen {
		w.p.urfaveEnd = end
	}
}

// flag returns the state of f in the walk.
func (w *walker) flag(f ucli.Flag) *lineFlag {
	if lf, ok := w.flags[f]; ok {
		return lf
	}
	lf := &lineFlag{names: f.Names()}
	if df, ok := f.(ucli.DocGenerationFlag); ok {
		lf.isBool = !df.TakesValue()
	}
	switch f := f.(type) {
	case *ucli.StringFlag:
		lf.once = f.OnlyOnce
	case *ucli.BoolFlag:
		lf.once = f.OnlyOnce
	case *ucli.StringSliceFlag:
		lf.once = f.OnlyOnce
	}
	w.flags[f] = lf
	return lf
}

// enter starts the level of the last command of chain, the commands from
// the root down, and returns it.
func (w *walker) enter(chain []*ucli.Command) *lineLevel {
	cmd := chain[len(chain)-1]
	lv := &lineLevel{cmd: cmd, leaf: len(cmd.Commands) == 0}
	w.p.levels = append(w.p.levels, lv)

	taken := map[string]bool{}
	for _, f := range cmd.Flags {
		lv.flags = append(lv.flags, w.flag(f))
		for _, name := range f.Names() {
			taken[name] = true
		}
	}
	if ucli.HelpFlag != nil && !slices.ContainsFunc(chain, func(c *ucli.Command) bool { return c.HideHelp }) {
		lv.help = builtinFlag(ucli.HelpFlag, taken)
	}
	if lv.help != nil {
		lv.flags = append(lv.flags, lv.help)
	}
	if len(chain) == 1 && cmd.Version != "" && !cmd.HideVersion && ucli.VersionFlag != nil {
		lv.version = builtinFlag(ucli.VersionFlag, taken)
	}
	if lv.version != nil {
		lv.flags = append(lv.flags, lv.version)
	}
	lv.own = len(lv.flags)

	for _, f := range lv.flags[:lv.own] {
		for _, name := range f.names {
			taken[name] = true
		}
	}
	for i := len(chain) - 2; i >= 0; i-- {
		for _, f := range chain[i].Flags {
			if lf, ok := f.(ucli.LocalFlag); !ok || lf.IsLocal() {
				continue
			}
			if slices.ContainsFunc(f.Names(), func(name string) bool { return taken[name] }) {
				continue
			}
			lv.flags = append(lv.flags, w.flag(f))
		}
	}
	return lv
}

// builtinFlag returns the state of a flag urfave adds to a command, the
// help flag or the version flag, or nil when a flag of the command already
// has one of its names. An alias of the flag that a flag of the command
// has is dropped, so the command's flag wins and the flag stays.
func builtinFlag(f ucli.Flag, taken map[string]bool) *lineFlag {
	names := f.Names()
	kept := names[:1:1]
	for _, alias := range names[1:] {
		if !taken[alias] {
			kept = append(kept, alias)
		}
	}
	if slices.ContainsFunc(kept, func(name string) bool { return taken[name] }) {
		return nil
	}
	return &lineFlag{names: kept, isBool: true, builtin: true}
}

// lookup returns the flag of the level named name, or nil.
func (lv *lineLevel) lookup(name string) *lineFlag {
	for _, f := range lv.flags {
		if slices.Contains(f.names, name) {
			return f
		}
	}
	return nil
}

// add records a word the level reads.
func (lv *lineLevel) add(word lineWord) {
	word.helpOn = lv.help != nil && lv.help.value
	word.versionOn = lv.version != nil && lv.version.set
	lv.words = append(lv.words, word)
}

// addUnread records the words a leaf takes as arguments without reading
// them.
func (lv *lineLevel) addUnread(rest []inWord) {
	for _, r := range rest {
		lv.add(lineWord{text: r.text, kind: plainWord, index: r.index, unread: true})
	}
}

// setResult is what setting a flag comes to.
type setResult int

const (
	setOK setResult = iota
	setRepeated
	setRejected
)

// apply sets the flag to val as urfave does: a flag that may be given once
// refuses a second time, and a bool flag refuses a value that is not a bool.
func (f *lineFlag) apply(val string) setResult {
	if f.once && f.count == 1 {
		return setRepeated
	}
	f.count++
	if f.isBool {
		v, err := strconv.ParseBool(val)
		if err != nil {
			return setRejected
		}
		f.value = v
	} else {
		f.text = val
	}
	f.set = true
	return setOK
}

// parseLevel reads in, the words handed to the level, as urfave reads them
// at a command, and returns the words it takes as arguments (for a command
// with subcommands, the words that go on to one) and the parse error it
// reports, if any.
func (w *walker) parseLevel(lv *lineLevel, in []inWord) (pos []inWord, class lineErrorClass) {
	stop := lv.cmd.StopOnNthArg
	for k := 0; k < len(in); k++ {
		word := in[k]
		c := classifyWord(word.text)
		typed := lineWord{text: word.text, kind: plainWord, index: word.index}
		switch c.class {
		case wordBlank:
			lv.add(typed)
			pos = append(pos, word)
			continue
		case wordEnd:
			lv.add(lineWord{text: c.trimmed, kind: endWord, index: word.index})
			pos = append(pos, in[k+1:]...)
			if lv.leaf {
				lv.addUnread(in[k+1:])
			}
			return pos, 0
		}
		if stop != nil && len(pos) == *stop {
			pos = append(pos, in[k:]...)
			return pos, 0
		}
		switch c.class {
		case wordPlain:
			lv.add(typed)
			pos = append(pos, word)
			if lv.cmd.Command(c.trimmed) != nil {
				pos = append(pos, in[k+1:]...)
				return pos, 0
			}
		case wordDash:
			typed.dash = true
			lv.add(typed)
			pos = append(pos, word)
		case wordNotFlag:
			lv.add(typed)
			pos = append(pos, in[k:]...)
			if lv.leaf {
				lv.addUnread(in[k+1:])
			}
			return pos, 0
		case wordFlag:
			if k, class = w.readFlagWord(lv, in, k, c); class != 0 {
				return pos, class
			}
		}
	}
	return pos, 0
}

// readFlagWord reads the flag word at in[k], with the separate value of a
// flag that takes one, and returns the index of the last word it read and
// the parse error urfave reports on it, if any.
func (w *walker) readFlagWord(lv *lineLevel, in []inWord, k int, c classifiedWord) (int, lineErrorClass) {
	f := lv.lookup(c.name)
	fw := lineWord{
		text:       c.trimmed,
		kind:       flagWord,
		name:       c.name,
		defined:    f != nil,
		takesValue: f != nil && !f.isBool,
		joined:     c.joined,
		value:      c.value,
		index:      in[k].index,
		flag:       f,
	}
	switch {
	case f == nil:
		lv.add(fw)
		return k, undefinedFlag
	case f.isBool:
		fw.blank, fw.empty = boolValueKind(fw.tailWord)
		lv.add(fw)
		val := c.value
		if val == "" {
			val = "true"
		}
		switch f.apply(val) {
		case setRepeated:
			return k, repeatedFlag
		case setRejected:
			if !fw.blank {
				return k, badBoolValue
			}
			w.blankWord(lv)
		}
		return k, 0
	}
	val := c.value
	if val == "" && !c.joined {
		if k == len(in)-1 {
			lv.add(fw)
			return k, missingFlagValue
		}
		k++
		val = in[k].text
		lv.add(fw)
		lv.add(lineWord{text: val, kind: valueWord, index: in[k].index})
	} else {
		lv.add(fw)
	}
	if f.apply(val) == setRepeated {
		return k, repeatedFlag
	}
	return k, 0
}

// blankWord records that the pass met a bool flag typed with "=" and a
// value of only whitespace, which urfave rejects: where it ends is where
// that word ends it. The walk goes on past it.
func (w *walker) blankWord(lv *lineLevel) {
	if w.blankSeen {
		return
	}
	w.blankSeen = true
	level := len(w.p.levels) - 1
	if lv.help != nil && lv.help.value {
		w.p.urfaveEnd = lineEnd{kind: lineHelp, level: level, afterError: true}
		return
	}
	w.p.urfaveEnd = lineEnd{kind: lineError, level: level, class: badBoolValue}
}

// readWholeLine reports whether the walk reached a leaf and read its whole
// line, so a -o, --output or --color typed anywhere on the line has been
// read. It did not, when the walk stopped at an error urfave reports, or at
// a root or a group that shows help, prints the version or has no command
// of the name typed: the words after that point were never read.
func (p parsedLine) readWholeLine() bool {
	end := p.endIfBlankAccepted
	return p.levels[end.level].leaf && end.kind != lineError && !end.afterError
}

// commandName returns the full name of the command of level i.
func (p parsedLine) commandName(i int) string {
	names := []string{"rancher"}
	for _, lv := range p.levels[1 : i+1] {
		names = append(names, lv.cmd.Name)
	}
	return strings.Join(names, " ")
}

// valueOf returns the value the walk read last for the value flag named
// name, as urfave sets it, or "".
func (p parsedLine) valueOf(name string) string {
	for _, f := range p.levels[0].flags[:p.levels[0].own] {
		if slices.Contains(f.names, name) {
			return f.text
		}
	}
	return ""
}

// isSet reports whether urfave's IsSet is true for the flag named name on
// the command of the level at index i: the first flag of that name among
// the command's own flags and its ancestors' is the one it reads.
func (p parsedLine) isSet(i int, name string) bool {
	for ; i >= 0; i-- {
		lv := p.levels[i]
		for _, f := range lv.flags[:lv.own] {
			if slices.Contains(f.names, name) {
				return f.set
			}
		}
	}
	return false
}
