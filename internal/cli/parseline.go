package cli

import (
	"slices"
	"strconv"
	"strings"
)

// lineFlag is one flag a command parses on a command line, with what the
// parser has read for it so far.
type lineFlag struct {
	// def is the flag of the command tree.
	def    *Flag
	names  []string
	isBool bool
	// slice is set for a flag that keeps one value for each time it is typed.
	slice bool
	// once is set for a flag that may be typed once, and builtin for the
	// help and version flags.
	once, builtin bool
	// count is how many times the flag was set, a value the flag rejects
	// included. set is true once a value the flag accepts was read, value is
	// a bool flag's value after the last such one, text a value flag's, and
	// values the values of a slice flag, one for each time it was typed.
	count  int
	set    bool
	value  bool
	text   string
	values []string
}

// secret reports whether the flag is marked Secret in the command tree.
func (f *lineFlag) secret() bool { return f.def != nil && f.def.Secret }

// lineWord is one argument on the command line as a command reads it, with
// the state of the command's help and version flags before it.
type lineWord struct {
	tailWord
	// index is the argument's place in the command line.
	index int
	// flag is the flag this argument names, when it names a defined flag.
	flag *lineFlag
	// helpOn is true when the command's help flag held true before this
	// argument, versionOn when the version flag was set before it.
	helpOn, versionOn bool
	// blank and empty mark a bool flag typed with "=" and a value that is
	// only whitespace, or nothing at all.
	blank, empty bool
	// unread marks an argument the command takes as its own without reading
	// it as a flag or a command name.
	unread bool
}

// lineLevel is one command on the path a command line takes from the root,
// as parsing reaches it.
type lineLevel struct {
	cmd *Command
	// flags are the flags the command parses: the first own of them are the
	// command's own, then the ones its ancestors apply to it.
	flags []*lineFlag
	own   int
	// help and version are the command's help flag and, at the root, version
	// flag; nil where the command has none.
	help, version *lineFlag
	// usageErr is the flag error that ends parsing at this command, when one
	// does.
	usageErr usageError
	// words are the arguments the command reads, in order, and args the
	// arguments it takes as its own, as typed, with their places in the
	// command line.
	words []lineWord
	args  []string
	argAt []int
	// leaf is true for a command with no subcommands.
	leaf bool
}

// lineEndKind says how parsing a command line ends.
type lineEndKind int

const (
	// lineRuns: the last command runs its action. A root or a group with an
	// argument left reports an unknown command; one without shows its help
	// page.
	lineRuns lineEndKind = iota
	// lineHelp: the last command shows its help page, as its help flag asks,
	// or after a flag error when the help flag came before it.
	lineHelp
	// lineVersion: the root prints the version.
	lineVersion
	// lineError: the last command reports a flag error.
	lineError
)

// lineErrorClass is the class of flag error a command reports.
type lineErrorClass int

const (
	undefinedFlag lineErrorClass = iota + 1
	missingFlagValue
	repeatedFlag
	badBoolValue
)

// lineEnd is where parsing a command line ends.
type lineEnd struct {
	kind lineEndKind
	// level is the index in parsedLine.levels of the last command reached.
	level int
	// afterError is true for a help page shown after a flag error: the
	// refusals then render in the format the whole command line selects
	// (readWholeLine). class is the class of the flag error; the dispatcher
	// builds the error text from the usageErr of that command, and leafOfLine
	// reads it to find an undefined flag.
	afterError bool
	class      lineErrorClass
}

// usageError is a flag error of a command, with what the error text names.
type usageError struct {
	class lineErrorClass
	// word is the name after the dashes of an undefined flag, and the argument
	// as typed of a flag that is missing its value.
	word string
	// typed is the flag as typed, with the whitespace around it cut off and
	// everything from the first "=" on: it never holds a value. It is set for
	// an undefined flag and for a flag with a bad value or a second typing.
	typed string
}

// parsedLine is a command line parsed once: the commands it reaches, the
// flags and arguments of each, and how it ends.
type parsedLine struct {
	argv   []string
	levels []*lineLevel
	// end is where parsing ends: the dispatcher, leafOfLine and the
	// recording of a run read it. endIfBlankAccepted is where parsing would
	// end if a bool flag typed with "=" and a value of only whitespace were
	// accepted and parsing went on: the same unless the command line has such
	// a flag, and the one the refusals read. levels are the commands reached
	// up to that end.
	end, endIfBlankAccepted lineEnd
	// helpOrVersionBeforeCommand is set by parseCompletion when a help or
	// version flag comes before the argument that names a subcommand.
	helpOrVersionBeforeCommand bool
	// readOnly and readOnlySources are the read-only state of this run: the
	// root's Before sets them (Input.setReadOnly) and buildIO reads them. For
	// a run whose root Before did not run, read-only mode is off and there are
	// no sources. Each run parses its own line: no other run reads them.
	readOnly        bool
	readOnlySources []string
}

// inWord is an argument handed to a command, with its place in the command
// line.
type inWord struct {
	text  string
	index int
}

// parseLine parses argv, the arguments typed after the program name, on
// root's command tree, after link ran. It does not change the tree: a flag's
// state is kept in the parser.
func parseLine(root *Command, argv []string) parsedLine {
	return parseWords(root, argv, false)
}

// parseCompletion parses the arguments before the one being completed. It
// reads them as parseLine does, except that an argument a run would refuse
// stays an argument or ends parsing at the command reached: an undefined flag
// stays one with every argument after it, a value flag with no value at the
// end stays one, and any other flag error ends parsing. A help or version
// flag does not end parsing; helpOrVersionBeforeCommand records that one came
// before a subcommand's name.
func parseCompletion(root *Command, argv []string) parsedLine {
	return parseWords(root, argv, true)
}

func parseWords(root *Command, argv []string, completion bool) parsedLine {
	w := &walker{flags: map[*Flag]*lineFlag{}, completion: completion}
	w.p.argv = argv
	in := make([]inWord, len(argv))
	for i, a := range argv {
		in[i] = inWord{a, i}
	}
	chain := []*Command{root}
	for {
		lv := w.enter(chain)
		pos, class := w.parseLevel(lv, in)
		at := len(w.p.levels) - 1
		for _, p := range pos {
			lv.args = append(lv.args, p.text)
			lv.argAt = append(lv.argAt, p.index)
		}
		switch {
		case class != 0 && !completion && lv.help != nil && lv.help.value:
			w.finish(lineEnd{kind: lineHelp, level: at, afterError: true})
		case class != 0:
			w.finish(lineEnd{kind: lineError, level: at, class: class})
		case !completion && lv.help != nil && lv.help.value:
			w.finish(lineEnd{kind: lineHelp, level: at})
		case !completion && lv.version != nil && lv.version.set:
			w.finish(lineEnd{kind: lineVersion, level: at})
		default:
			if len(pos) > 0 {
				if sub := lv.cmd.Command(pos[0].text); sub != nil {
					if completion && (lv.help != nil && lv.help.value || lv.version != nil && lv.version.set) {
						w.p.helpOrVersionBeforeCommand = true
					}
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

// walker holds the state of parsing one command line.
type walker struct {
	p parsedLine
	// completion is true when parsing a completion request.
	completion bool
	// flags maps each flag of the tree the parser meets to its state. A flag a
	// subcommand inherits is one flag at every command.
	flags map[*Flag]*lineFlag
	// blankSeen is true once parsing met a bool flag typed with "=" and a
	// value of only whitespace.
	blankSeen bool
}

// finish records where parsing ends.
func (w *walker) finish(end lineEnd) {
	w.p.endIfBlankAccepted = end
	if !w.blankSeen {
		w.p.end = end
	}
}

// flag returns the state of f in this parse.
func (w *walker) flag(f *Flag) *lineFlag {
	if lf, ok := w.flags[f]; ok {
		return lf
	}
	lf := &lineFlag{
		def:     f,
		names:   f.Names(),
		isBool:  !f.takesValue(),
		slice:   f.Kind == sliceFlag,
		once:    f.Once,
		builtin: f.Local,
	}
	w.flags[f] = lf
	return lf
}

// enter starts parsing the last command of chain, the commands from the root
// down, and returns its lineLevel.
func (w *walker) enter(chain []*Command) *lineLevel {
	cmd := chain[len(chain)-1]
	lv := &lineLevel{cmd: cmd, leaf: len(cmd.Commands) == 0}
	w.p.levels = append(w.p.levels, lv)

	taken := map[string]bool{}
	for _, f := range cmd.Flags {
		lf := w.flag(f)
		lv.flags = append(lv.flags, lf)
		for _, name := range f.Names() {
			taken[name] = true
		}
		if f.Local && f.Name == "help" {
			lv.help = lf
		}
		if f.Local && f.Name == "version" && len(chain) == 1 {
			lv.version = lf
		}
	}
	lv.own = len(lv.flags)

	for i := len(chain) - 2; i >= 0; i-- {
		for _, f := range chain[i].Flags {
			if f.Local {
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

// lookup returns the flag of the command named name, or nil.
func (lv *lineLevel) lookup(name string) *lineFlag {
	for _, f := range lv.flags {
		if slices.Contains(f.names, name) {
			return f
		}
	}
	return nil
}

// add records an argument the command reads.
func (lv *lineLevel) add(word lineWord) {
	word.helpOn = lv.help != nil && lv.help.value
	word.versionOn = lv.version != nil && lv.version.set
	lv.words = append(lv.words, word)
}

// addUnread records the arguments a leaf takes as its own without reading
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

// apply sets the flag to val: a flag that may be typed once refuses a second
// typing, and a bool flag refuses a value that is not a bool. A slice flag
// keeps every value, whole.
func (f *lineFlag) apply(val string) setResult {
	if f.once && f.count == 1 {
		return setRepeated
	}
	f.count++
	switch {
	case f.isBool:
		v, err := strconv.ParseBool(val)
		if err != nil {
			return setRejected
		}
		f.value = v
	case f.slice:
		f.values = append(f.values, val)
	default:
		f.text = val
	}
	f.set = true
	return setOK
}

// parseLevel reads in, the arguments handed to the command, and returns the
// arguments it takes as its own (for a command with subcommands, the
// arguments that go on to one) and the flag error it reports, if any.
func (w *walker) parseLevel(lv *lineLevel, in []inWord) (pos []inWord, class lineErrorClass) {
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
		if lv.cmd.StopAtFirstWord && len(pos) == 1 {
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
			var asArgs bool
			if k, class, asArgs = w.readFlagWord(lv, in, k, c); class != 0 {
				return pos, class
			} else if asArgs {
				pos = append(pos, in[k:]...)
				return pos, 0
			}
		}
	}
	return pos, 0
}

// readFlagWord reads the flag at in[k], with the separate value of a flag
// that takes one, and returns the index of the last argument it read and the
// flag error it ends with, if any. When parsing a completion request, asArgs
// is true for an argument that is not read as a flag: the arguments from
// in[k] on stay arguments.
func (w *walker) readFlagWord(lv *lineLevel, in []inWord, k int, c classifiedWord) (next int, class lineErrorClass, asArgs bool) {
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
		if w.completion {
			return k, 0, true
		}
		lv.usageErr = usageError{class: undefinedFlag, word: c.name, typed: typedFlag(c)}
		return k, undefinedFlag, false
	case f.isBool:
		fw.blank, fw.empty = boolValueKind(fw.tailWord)
		lv.add(fw)
		val := c.value
		if val == "" {
			val = "true"
		}
		bad := usageError{class: badBoolValue, typed: typedFlag(c)}
		switch f.apply(val) {
		case setRepeated:
			lv.usageErr = usageError{class: repeatedFlag, typed: typedFlag(c)}
			return k, repeatedFlag, false
		case setRejected:
			lv.usageErr = bad
			if !fw.blank {
				return k, badBoolValue, false
			}
			w.blankWord(lv)
		}
		return k, 0, false
	}
	val := c.value
	if val == "" && !c.joined {
		if k == len(in)-1 {
			lv.add(fw)
			if w.completion {
				return k, 0, true
			}
			lv.usageErr = usageError{class: missingFlagValue, word: c.trimmed}
			return k, missingFlagValue, false
		}
		k++
		val = in[k].text
		lv.add(fw)
		lv.add(lineWord{text: val, kind: valueWord, index: in[k].index})
	} else {
		lv.add(fw)
	}
	if f.apply(val) == setRepeated {
		lv.usageErr = usageError{class: repeatedFlag, typed: typedFlag(c)}
		return k, repeatedFlag, false
	}
	return k, 0, false
}

// typedFlag is the flag word c as typed, up to the first "=": the dashes and
// the name, never a value.
func typedFlag(c classifiedWord) string {
	typed, _, _ := strings.Cut(c.trimmed, "=")
	return typed
}

// blankWord records that parsing met a bool flag typed with "=" and a value
// of only whitespace, which the flag rejects: end holds where that flag ends
// parsing. Parsing goes on past it.
func (w *walker) blankWord(lv *lineLevel) {
	if w.blankSeen {
		return
	}
	w.blankSeen = true
	level := len(w.p.levels) - 1
	if lv.help != nil && lv.help.value {
		w.p.end = lineEnd{kind: lineHelp, level: level, afterError: true}
		return
	}
	w.p.end = lineEnd{kind: lineError, level: level, class: badBoolValue}
}

// readWholeLine reports whether parsing reached a leaf and read the whole
// command line: a -o, --output or --color typed anywhere on it has been
// read. It did not, when parsing stopped at a flag error it reports, or at a
// root or a group that shows a help page, prints the version or has no
// command of the name typed: the arguments after that point were never read.
func (p parsedLine) readWholeLine() bool {
	end := p.endIfBlankAccepted
	return p.levels[end.level].leaf && end.kind != lineError && !end.afterError
}

// commandName returns the full name of the command at index i of p.levels.
func (p parsedLine) commandName(i int) string {
	names := []string{"rancher"}
	for _, lv := range p.levels[1 : i+1] {
		names = append(names, lv.cmd.Name)
	}
	return strings.Join(names, " ")
}

// valueOf returns the value parsing read last for the value flag named name,
// or "".
func (p parsedLine) valueOf(name string) string {
	for _, f := range p.levels[0].flags[:p.levels[0].own] {
		if slices.Contains(f.names, name) {
			return f.text
		}
	}
	return ""
}
