package clierr

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markedDetails is a details payload that holds a marked command and
// implements both mapping interfaces, the value a field of type any holds
// when its owner has to map it.
type markedDetails struct{ Text string }

func (d markedDetails) TextDetails() []DetailLine { return []DetailLine{Line(ValuePart("", d.Text))} }

func (d markedDetails) MapText(f func(string) string) TextDetails {
	d.Text = f(d.Text)
	return d
}

func (d markedDetails) MapCommands(f func(string) string) TextDetails {
	d.Text = f(d.Text)
	return d
}

// errorPointer is the type of a nested error, which is one text field of its
// owner: its message stands for the whole error, which is never mapped.
var errorPointer = reflect.TypeFor[*Error]()

// textPaths is the path of every field of typ that holds text: a string, an
// interface that can hold anything, or a nested *Error, reaching through
// struct fields, pointers, slices, arrays and maps (a map's key and value
// both). A path joins the field names with a dot, with [] for a slice or
// array element, {} for a map value and {key} for a map key. A type that
// contains itself is walked once.
func textPaths(typ reflect.Type) []string {
	var out []string
	var walk func(t reflect.Type, path string, stack []reflect.Type)
	walk = func(t reflect.Type, path string, stack []reflect.Type) {
		switch t.Kind() {
		case reflect.String, reflect.Interface:
			out = append(out, path)
		case reflect.Pointer:
			if t == errorPointer {
				out = append(out, path)
				return
			}
			walk(t.Elem(), path, stack)
		case reflect.Slice, reflect.Array:
			walk(t.Elem(), path+"[]", stack)
		case reflect.Map:
			walk(t.Key(), path+"{key}", stack)
			walk(t.Elem(), path+"{}", stack)
		case reflect.Struct:
			if slices.Contains(stack, t) {
				return
			}
			for f := range t.Fields() {
				name := f.Name
				if path != "" {
					name = path + "." + name
				}
				walk(f.Type, name, append(stack, t))
			}
		}
	}
	walk(typ, "", nil)
	return out
}

// plantMarked is a value of typ with a marked command in every text field,
// each command naming the field's path. It fails the test for a text field
// it cannot set, an unexported one.
func plantMarked(t *testing.T, typ reflect.Type, path string, stack []reflect.Type) reflect.Value {
	t.Helper()
	switch typ.Kind() {
	case reflect.String:
		v := reflect.New(typ).Elem()
		v.SetString(Rancher + " plant " + path)
		return v
	case reflect.Interface:
		v := reflect.New(typ).Elem()
		v.Set(reflect.ValueOf(markedDetails{Text: Rancher + " plant " + path}))
		return v
	case reflect.Pointer:
		if typ == errorPointer {
			return reflect.ValueOf(New(CodeGeneric, Rancher+" plant "+path))
		}
		v := reflect.New(typ.Elem())
		if !slices.Contains(stack, typ.Elem()) {
			v.Elem().Set(plantMarked(t, typ.Elem(), path, stack))
		}
		return v
	case reflect.Slice:
		v := reflect.MakeSlice(typ, 1, 1)
		v.Index(0).Set(plantMarked(t, typ.Elem(), path+"[]", stack))
		return v
	case reflect.Array:
		v := reflect.New(typ).Elem()
		for i := range v.Len() {
			v.Index(i).Set(plantMarked(t, typ.Elem(), path+"[]", stack))
		}
		return v
	case reflect.Map:
		v := reflect.MakeMap(typ)
		v.SetMapIndex(plantMarked(t, typ.Key(), path+"{key}", stack), plantMarked(t, typ.Elem(), path+"{}", stack))
		return v
	case reflect.Struct:
		v := reflect.New(typ).Elem()
		if slices.Contains(stack, typ) {
			return v
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			name := f.Name
			if path != "" {
				name = path + "." + name
			}
			if !f.IsExported() {
				if len(textPaths(f.Type)) > 0 {
					require.Failf(t, "unexported text field", "%s.%s cannot be planted; export it or classify it by hand", typ, f.Name)
				}
				continue
			}
			v.Field(i).Set(plantMarked(t, f.Type, name, append(stack, typ)))
		}
		return v
	default:
		return reflect.New(typ).Elem()
	}
}

// textValues is the value of every text field v holds, by path, for the
// paths textPaths lists. A field of type any holding markedDetails gives
// its text.
func textValues(v reflect.Value, path string, out map[string]string) {
	switch v.Kind() {
	case reflect.String:
		out[path] = v.String()
	case reflect.Interface:
		if md, ok := reflect.TypeAssert[markedDetails](v); ok {
			out[path] = md.Text
		}
	case reflect.Pointer:
		switch {
		case v.IsNil():
		case v.Type() == errorPointer:
			out[path] = v.Interface().(*Error).Message
		default:
			textValues(v.Elem(), path, out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			textValues(v.Index(i), path+"[]", out)
		}
	case reflect.Map:
		for iter := v.MapRange(); iter.Next(); {
			textValues(iter.Key(), path+"{key}", out)
			textValues(iter.Value(), path+"{}", out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			name := v.Type().Field(i).Name
			if path != "" {
				name = path + "." + name
			}
			textValues(v.Field(i), name, out)
		}
	}
}

// textFieldLists is how a details type classifies every field that holds
// text, by path: the fields that can hold a remedy command, which its
// mapping rewrites; the fields that hold the server's or the user's text,
// which it never rewrites; and the other text the CLI writes, which holds
// no command.
type textFieldLists struct {
	remedy, server, other []string
}

func (l textFieldLists) all() []string { return slices.Concat(l.remedy, l.server, l.other) }

// assertEveryFieldClassified asserts every text field of typ is in exactly
// one of the three lists and every entry names a text field of typ: a field
// added later and not classified fails.
func assertEveryFieldClassified(t *testing.T, typ reflect.Type, lists textFieldLists) {
	t.Helper()
	all := textPaths(typ)
	listed := lists.all()
	for _, path := range all {
		n := 0
		for _, entry := range listed {
			if entry == path {
				n++
			}
		}
		assert.Equal(t, 1, n, "%s: the text field %s is in exactly one list", typ, path)
	}
	for _, entry := range listed {
		assert.Contains(t, all, entry, "%s: the entry %s names no text field", typ, entry)
	}
}

// assertMappedAsListed asserts, for each path the three lists name, that got
// holds the planted command expanded with prefix when the path is in mapped
// and as planted otherwise.
func assertMappedAsListed(t *testing.T, planted, got any, lists textFieldLists, mapped []string, prefix string) {
	t.Helper()
	before, after := map[string]string{}, map[string]string{}
	textValues(reflect.ValueOf(planted), "", before)
	textValues(reflect.ValueOf(got), "", after)
	for _, path := range lists.all() {
		was, ok := before[path]
		require.True(t, ok, "the planted value holds the field %s", path)
		if slices.Contains(mapped, path) {
			assert.Equal(t, strings.ReplaceAll(was, Rancher, "rancher"+prefix), after[path], "%s is mapped", path)
			assert.NotEqual(t, was, after[path], "%s is mapped", path)
		} else {
			assert.Equal(t, was, after[path], "%s is left as it is", path)
		}
	}
}
