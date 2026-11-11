package remedytest

import (
	"crypto/x509"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// MarkedDetails is a details payload that holds a marked command and
// implements both mapping interfaces: the value a field of type any holds
// when its owner has to map it.
type MarkedDetails struct{ Text string }

// TextDetails is the one line the payload holds, its text as one value.
func (d MarkedDetails) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("", d.Text))}
}

// MapText maps the text.
func (d MarkedDetails) MapText(f func(string) string) clierr.TextDetails {
	d.Text = f(d.Text)
	return d
}

// MapCommands maps the text.
func (d MarkedDetails) MapCommands(f func(string) string) clierr.TextDetails {
	d.Text = f(d.Text)
	return d
}

// DetailTexts is each line of a details payload as plain text, with every
// value written as it is: the text a test compares with a string.
func DetailTexts(lines []clierr.DetailLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.String()
	}
	return out
}

// errorPointer is the type of a nested error, which is one text field of its
// owner: its message stands for the whole error, which is never mapped.
var errorPointer = reflect.TypeFor[*clierr.Error]()

// certificatePointer is the type of a certificate a server presented, which an
// owner keeps unexported and never serializes: none of its fields holds
// text the CLI writes; it has no text field.
var certificatePointer = reflect.TypeFor[*x509.Certificate]()

// TextPaths is the path of every field of typ that holds text: a string, an
// interface that can hold anything, or a nested *clierr.Error, reaching
// through struct fields, pointers, slices, arrays and maps (a map's key and
// value both). A path joins the field names with a dot, with [] for a slice
// or array element, {} for a map value and {key} for a map key. A type that
// contains itself is walked once.
func TextPaths(typ reflect.Type) []string {
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
			if t == certificatePointer {
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

// PlantMarked is a value of typ with a marked command in every text field,
// each command naming the field's path. It fails the test for a text field
// it cannot set, an unexported one.
func PlantMarked(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	return plant(t, typ, "", nil, markedLeaves())
}

// PlantText is a value of typ with value(path) in every text field: a
// string field, a field of an interface type (which holds the string), and a
// nested *clierr.Error (whose message is the string). It fails the test for a
// text field it cannot set, an unexported one.
func PlantText(t *testing.T, typ reflect.Type, value func(path string) string) reflect.Value {
	t.Helper()
	return plant(t, typ, "", nil, leaves{
		text:  value,
		iface: func(path string) any { return value(path) },
		err:   func(path string) *clierr.Error { return clierr.New(clierr.CodeGeneric, value(path)) },
	})
}

// leaves is what a planter puts in each kind of text field.
type leaves struct {
	text  func(path string) string
	iface func(path string) any
	err   func(path string) *clierr.Error
}

// markedLeaves plant a marked command naming the path.
func markedLeaves() leaves {
	return leaves{
		text:  func(path string) string { return clierr.Rancher + " plant " + path },
		iface: func(path string) any { return MarkedDetails{Text: clierr.Rancher + " plant " + path} },
		err:   func(path string) *clierr.Error { return clierr.New(clierr.CodeGeneric, clierr.Rancher+" plant "+path) },
	}
}

func plant(t *testing.T, typ reflect.Type, path string, stack []reflect.Type, leaf leaves) reflect.Value {
	t.Helper()
	switch typ.Kind() {
	case reflect.String:
		v := reflect.New(typ).Elem()
		v.SetString(leaf.text(path))
		return v
	case reflect.Interface:
		v := reflect.New(typ).Elem()
		v.Set(reflect.ValueOf(leaf.iface(path)))
		return v
	case reflect.Pointer:
		if typ == errorPointer {
			return reflect.ValueOf(leaf.err(path))
		}
		v := reflect.New(typ.Elem())
		if !slices.Contains(stack, typ.Elem()) {
			v.Elem().Set(plant(t, typ.Elem(), path, stack, leaf))
		}
		return v
	case reflect.Slice:
		v := reflect.MakeSlice(typ, 1, 1)
		v.Index(0).Set(plant(t, typ.Elem(), path+"[]", stack, leaf))
		return v
	case reflect.Array:
		v := reflect.New(typ).Elem()
		for i := range v.Len() {
			v.Index(i).Set(plant(t, typ.Elem(), path+"[]", stack, leaf))
		}
		return v
	case reflect.Map:
		v := reflect.MakeMap(typ)
		v.SetMapIndex(plant(t, typ.Key(), path+"{key}", stack, leaf), plant(t, typ.Elem(), path+"{}", stack, leaf))
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
				if len(TextPaths(f.Type)) > 0 {
					require.Failf(t, "unexported text field", "%s.%s cannot be planted; export it or classify it by hand", typ, f.Name)
				}
				continue
			}
			v.Field(i).Set(plant(t, f.Type, name, append(stack, typ), leaf))
		}
		return v
	default:
		return reflect.New(typ).Elem()
	}
}

// TextValues is the value of every text field v holds, by path, for the
// paths TextPaths lists. A field of type any holding MarkedDetails gives its
// text.
func TextValues(v reflect.Value) map[string]string {
	out := map[string]string{}
	collect(v, "", out)
	return out
}

func collect(v reflect.Value, path string, out map[string]string) {
	switch v.Kind() {
	case reflect.String:
		out[path] = v.String()
	case reflect.Interface:
		if md, ok := reflect.TypeAssert[MarkedDetails](v); ok {
			out[path] = md.Text
		}
	case reflect.Pointer:
		switch {
		case v.IsNil():
		case v.Type() == errorPointer:
			if e, ok := reflect.TypeAssert[*clierr.Error](v); ok {
				out[path] = e.Message
			}
		default:
			collect(v.Elem(), path, out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			collect(v.Index(i), path+"[]", out)
		}
	case reflect.Map:
		for iter := v.MapRange(); iter.Next(); {
			collect(iter.Key(), path+"{key}", out)
			collect(iter.Value(), path+"{}", out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			name := v.Type().Field(i).Name
			if path != "" {
				name = path + "." + name
			}
			collect(v.Field(i), name, out)
		}
	}
}

// FieldLists is how a details type classifies every field that holds text,
// by path: the fields that can hold a remedy command, which its mapping
// rewrites; the fields that hold the server's or the user's text, which it
// never rewrites; and the other text the CLI writes, which holds no command.
type FieldLists struct {
	Remedy, Server, Other []string
}

// All is the three lists together.
func (l FieldLists) All() []string { return slices.Concat(l.Remedy, l.Server, l.Other) }

// AssertEveryFieldClassified asserts every text field of typ is in exactly
// one of the three lists and every entry names a text field of typ: a field
// added later and not classified fails.
func AssertEveryFieldClassified(t *testing.T, typ reflect.Type, lists FieldLists) {
	t.Helper()
	all := TextPaths(typ)
	listed := lists.All()
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

// AssertMappedAsListed asserts, for each path the three lists name, that got
// holds the planted command expanded with prefix when the path is in mapped
// and as planted otherwise.
func AssertMappedAsListed(t *testing.T, planted, got any, lists FieldLists, mapped []string, prefix string) {
	t.Helper()
	before, after := TextValues(reflect.ValueOf(planted)), TextValues(reflect.ValueOf(got))
	for _, path := range lists.All() {
		was, ok := before[path]
		require.True(t, ok, "the planted value holds the field %s", path)
		if slices.Contains(mapped, path) {
			assert.Equal(t, strings.ReplaceAll(was, clierr.Rancher, "rancher"+prefix), after[path], "%s is mapped", path)
			assert.NotEqual(t, was, after[path], "%s is mapped", path)
		} else {
			assert.Equal(t, was, after[path], "%s is left as it is", path)
		}
	}
}
