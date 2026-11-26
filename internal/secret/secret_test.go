package secret_test

import (
	"bytes"
	"encoding"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/secret"
)

const planted = "PLANT-s3cr3t/value+9"

const plantedOther = "OTHER-t0p-s3cret=42"

// textForms returns every form the bytes of text print in: the text, its
// decimal byte list, its hex in lower and upper case (joined and spaced), its
// Go byte literal and its base64 encodings.
func textForms(text string) []string {
	b := []byte(text)
	dec := make([]string, len(b))
	lit := make([]string, len(b))
	for i, c := range b {
		dec[i] = strconv.Itoa(int(c))
		lit[i] = fmt.Sprintf("0x%02x", c)
	}
	return []string{
		text,
		strings.Join(dec, " "),
		strings.Join(dec, ","),
		hex.EncodeToString(b),
		strings.ToUpper(hex.EncodeToString(b)),
		fmt.Sprintf("% x", b),
		fmt.Sprintf("% X", b),
		strings.Join(lit, ", "),
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
	}
}

// assertHidesText asserts out has no form of the planted texts.
func assertHidesText(t *testing.T, out, where string) {
	t.Helper()
	for _, text := range []string{planted, plantedOther} {
		for _, form := range textForms(text) {
			assert.NotContains(t, out, form, where)
		}
	}
}

type exportedHolder struct{ V secret.Value }

type unexportedHolder struct{ v secret.Value }

type pointerHolder struct{ p *secret.Value }

type anyHolder struct{ x any }

type deepHolder struct {
	A struct {
		B struct {
			V secret.Value
			v secret.Value
		}
	}
}

// wordShape has the shape of the fieldWord of the cloud credential writes.
type wordShape struct {
	name, value secret.Value
	source      string
}

// positions returns the planted value at every position a printer reaches it
// from.
func positions() map[string]any {
	v := secret.New(planted)
	o := secret.New(plantedOther)
	deep := deepHolder{}
	deep.A.B.V = v
	deep.A.B.v = o
	return map[string]any{
		"top":                v,
		"pointer":            &v,
		"exported field":     exportedHolder{v},
		"exported pointer":   &exportedHolder{v},
		"unexported field":   unexportedHolder{v},
		"unexported pointer": &unexportedHolder{v},
		"pointer field":      pointerHolder{&v},
		"any field":          anyHolder{v},
		"two fields deep":    deep,
		"slice":              []secret.Value{v, o},
		"slice of pointers":  []*secret.Value{&v, nil},
		"map":                map[string]any{"k": v},
		"map of values":      map[string]secret.Value{"k": v},
		"word":               wordShape{v, o, "x"},
		"word pointer":       &wordShape{v, o, "x"},
		"array":              [1]secret.Value{v},
		"nil pointer":        (*secret.Value)(nil),
	}
}

var verbs = []string{
	"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%p", "%w", "%t", "%e", "%c",
	"%U", "%o", "%b", "%8.3s", "%-#20x", "%T", "% x", "%10v", "%-10v|", "%.2v", "%+q", "%#x",
}

// TestValuePrintsHidden asserts every printer and encoder writes <hidden> for
// a planted value at every position, and that no output has the text, its
// decimal byte list, its hex in either case, its Go byte literal or its
// base64: fmt with each verb, encoding/json, YAML, log/slog, templates and
// testify's failure messages.
func TestValuePrintsHidden(t *testing.T) {
	t.Parallel()
	t.Run("fmt", checkFmtPrinters)
	t.Run("encoders", checkEncoders)
	t.Run("slog", checkSlog)
	t.Run("templates", checkTemplates)
	t.Run("testify messages", checkAssertionMessages)
}

// checkFmtPrinters asserts every printer writes <hidden> for a planted
// value at every position, and that no output has the text, its decimal
// byte list, its hex in either case, its Go byte literal or its base64.
func checkFmtPrinters(t *testing.T) {
	t.Helper()
	for name, x := range positions() {
		for _, verb := range verbs {
			where := name + " " + verb
			assertHidesText(t, fmt.Sprintf(verb, x), where+" Sprintf")
			assertHidesText(t, fmt.Errorf(verb, x).Error(), where+" Errorf")
			assertHidesText(t, fmt.Sprintf(verb+verb, x, x), where+" Sprintf twice")
		}
		assertHidesText(t, fmt.Sprint(x), name+" Sprint")
		assertHidesText(t, fmt.Sprintln(x), name+" Sprintln")
		assertHidesText(t, fmt.Sprint("a", x, "b", x), name+" Sprint mixed")
		var buf bytes.Buffer
		_, _ = fmt.Fprint(&buf, x)
		_, _ = fmt.Fprintf(&buf, "%v %+v %#v", x, x, x)
		assertHidesText(t, buf.String(), name+" Fprint")
	}
}

// TestValuePrintsExactly asserts the text each verb writes for a value at the
// top, behind a pointer and inside a value.
func TestValuePrintsExactly(t *testing.T) {
	t.Parallel()
	v := secret.New(planted)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%.2v", "%5.1s"} {
		assert.Equal(t, "<hidden>", fmt.Sprintf(verb, v), verb)
		assert.Equal(t, "<hidden>", fmt.Sprintf(verb, &v), verb+" pointer")
	}
	assert.Equal(t, "  <hidden>", fmt.Sprintf("%10v", v))
	assert.Equal(t, "<hidden>  |", fmt.Sprintf("%-10v|", v))
	assert.Equal(t, "secret.Value", fmt.Sprintf("%T", v))
	assert.Equal(t, "<hidden>", v.String())
	assert.Equal(t, "<hidden>", v.GoString())
	assert.Equal(t, "{<hidden>}", fmt.Sprintf("%v", exportedHolder{v}))
	assert.Equal(t, "[<hidden> <hidden>]", fmt.Sprintf("%v", []secret.Value{v, v}))
	assert.Equal(t, "<nil>", fmt.Sprintf("%v", (*secret.Value)(nil)))
	assert.Equal(t, "<hidden>", fmt.Sprint(secret.Value{}))
}

// checkEncoders asserts the encoders write <hidden> for a value in
// every position they reach, and leave an unexported field out.
func checkEncoders(t *testing.T) {
	t.Helper()
	v := secret.New(planted)
	o := secret.New(plantedOther)
	cases := map[string]any{
		"top":      v,
		"pointer":  &v,
		"field":    exportedHolder{v},
		"slice":    []secret.Value{v, o},
		"map":      map[string]secret.Value{"k": v},
		"any map":  map[string]any{"k": v, "n": &o},
		"deep":     positions()["two fields deep"],
		"word":     wordShape{v, o, "x"},
		"nil":      (*secret.Value)(nil),
		"nil slot": []*secret.Value{nil, &v},
	}
	for name, x := range cases {
		out, err := json.Marshal(x)
		require.NoError(t, err, name)
		assertHidesText(t, string(out), name+" json")
		out, err = json.MarshalIndent(x, "", " ")
		require.NoError(t, err, name)
		assertHidesText(t, string(out), name+" json indent")
		out, err = yaml.Marshal(x)
		require.NoError(t, err, name)
		assertHidesText(t, string(out), name+" yaml.v3")
		out, err = sigsyaml.Marshal(x)
		require.NoError(t, err, name)
		assertHidesText(t, string(out), name+" sigs yaml")
	}
	out, err := json.Marshal(v)
	require.NoError(t, err)
	assert.JSONEq(t, `"<hidden>"`, string(out))
	out, err = json.Marshal(exportedHolder{v})
	require.NoError(t, err)
	assert.JSONEq(t, `{"V":"<hidden>"}`, string(out))
	for _, row := range []struct {
		x    any
		want string
	}{
		{unexportedHolder{v}, `{}`},
		{[]unexportedHolder{{v}}, `[{}]`},
	} {
		out, err = json.Marshal(row.x)
		require.NoError(t, err)
		assert.JSONEq(t, row.want, string(out))
	}
	out, err = json.Marshal(map[string]any{"k": v})
	require.NoError(t, err)
	assert.JSONEq(t, `{"k":"<hidden>"}`, string(out))
	out, err = yaml.Marshal(struct{ V secret.Value }{v})
	require.NoError(t, err)
	assert.Equal(t, "v: <hidden>\n", string(out))

	var marshaler encoding.TextMarshaler = v
	text, err := marshaler.MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "<hidden>", string(text))
	assert.Equal(t, "<hidden>", v.MarshalLog())
	assert.Equal(t, "<hidden>", v.LogValue().String())
}

// checkSlog asserts log/slog writes <hidden> for a value, as an
// attribute, inside a group and through slog.Any.
func checkSlog(t *testing.T) {
	t.Helper()
	v := secret.New(planted)
	for _, newHandler := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buf bytes.Buffer
		log := slog.New(newHandler(&buf))
		log.Info("message", "v", v, slog.Any("p", &v), slog.Group("g", slog.Any("v", v)), "word", wordShape{v, v, "x"}, "holder", exportedHolder{v})
		assertHidesText(t, buf.String(), "slog")
		assert.Contains(t, buf.String(), "<hidden>")
	}
}

// checkTemplates asserts a template prints <hidden> for a value and
// reaches no method that returns the text.
func checkTemplates(t *testing.T) {
	t.Helper()
	v := secret.New(planted)
	for _, text := range []string{
		"{{.}}", "{{.V}}", "{{.String}}", "{{.V.String}}", `{{printf "%s %v %q %x" .V .V .V .V}}`,
		"{{.V.GoString}}", "{{.V.MarshalLog}}", "{{.V.Len}}", "{{index .L 0}}", `{{index .M "k"}}`, "{{range .L}}{{.}}{{end}}",
	} {
		tpl, err := template.New("t").Parse(text)
		require.NoError(t, err, text)
		var buf bytes.Buffer
		data := map[string]any{"V": v, "L": []secret.Value{v}, "M": map[string]secret.Value{"k": v}}
		if text == "{{.}}" || text == "{{.String}}" {
			require.NoError(t, tpl.Execute(&buf, v), text)
		} else {
			require.NoError(t, tpl.Execute(&buf, data), text)
		}
		assertHidesText(t, buf.String(), text)
	}
}

// recordingT records what testify would report.
type recordingT struct{ messages []string }

func (r *recordingT) Errorf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

// checkAssertionMessages asserts testify's message for a failed
// comparison of two values prints <hidden>. The diff of testify prints the
// text through go-spew, with test values only, and is left out.
func checkAssertionMessages(t *testing.T) {
	t.Helper()
	a, b := secret.New(planted), secret.New(plantedOther)
	cases := map[string][2]any{
		"values":   {a, b},
		"pointers": {&a, &b},
		"holders":  {exportedHolder{a}, exportedHolder{b}},
		"slices":   {[]secret.Value{a}, []secret.Value{b}},
	}
	for name, pair := range cases {
		rec := &recordingT{}
		assert.Equal(rec, pair[0], pair[1])
		require.NotEmpty(t, rec.messages, name)
		for _, m := range rec.messages {
			message, _, _ := strings.Cut(m, "Diff:")
			assertHidesText(t, message, name)
		}
	}
}

// TestValueIsNotComparable asserts == and use as a map key do not compile and
// the struct adds no size to the pointer it has.
func TestValueIsNotComparable(t *testing.T) {
	t.Parallel()
	assert.False(t, reflect.TypeFor[secret.Value]().Comparable())
	assert.Equal(t, unsafe.Sizeof(uintptr(0)), unsafe.Sizeof(secret.Value{}))
}

func TestValueNew(t *testing.T) {
	t.Parallel()
	v := secret.New(planted)
	assert.Equal(t, planted, secret.Reveal(v))
	assert.Equal(t, len(planted), v.Len())
	assert.False(t, v.IsZero())
	assert.False(t, v.IsBlank())
	assert.True(t, v.EqualString(planted))
	assert.False(t, v.EqualString("x"))
	assert.True(t, v.EqualFoldString(strings.ToUpper(planted)))
	assert.False(t, v.EqualFoldString("x"))
	assert.True(t, v.Equal(secret.New(planted)))
	assert.False(t, v.Equal(secret.New(plantedOther)))
}

func TestValueZero(t *testing.T) {
	t.Parallel()
	var v secret.Value
	assert.Equal(t, "", secret.Reveal(v))
	assert.Equal(t, 0, v.Len())
	assert.True(t, v.IsZero())
	assert.True(t, v.IsBlank())
	assert.True(t, v.Equal(secret.New("")))
	assert.True(t, v.EqualString(""))
	assert.True(t, secret.New("").IsZero())
}

func TestValueIsBlank(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{"": true, " ": true, " \t\r\n": true, " a ": false, "a": false} {
		assert.Equal(t, want, secret.New(in).IsBlank(), strconv.Quote(in))
	}
}

func TestValueFromBytesCopies(t *testing.T) {
	t.Parallel()
	b := []byte(planted)
	v := secret.FromBytes(b)
	clear(b)
	assert.Equal(t, planted, secret.Reveal(v))
}

func TestValueCut(t *testing.T) {
	t.Parallel()
	before, after, found := secret.New("a=b=c").Cut("=")
	assert.True(t, found)
	assert.Equal(t, "a", secret.Reveal(before))
	assert.Equal(t, "b=c", secret.Reveal(after))
	before, after, found = secret.New("abc").Cut("=")
	assert.False(t, found)
	assert.Equal(t, "abc", secret.Reveal(before))
	assert.True(t, after.IsZero())
	before, after, found = secret.New(`kubeconfigContent=C:\Users\me\kc.yaml`).Cut("=")
	assert.True(t, found)
	assert.Equal(t, "kubeconfigContent", secret.Reveal(before))
	assert.Equal(t, `C:\Users\me\kc.yaml`, secret.Reveal(after))
}

func TestValueReadClearsTheBuffer(t *testing.T) {
	t.Parallel()
	buf := []byte(planted)
	var gotPath string
	v, err := secret.Read(func(path string) ([]byte, error) {
		gotPath = path
		return buf, nil
	}, "file")
	require.NoError(t, err)
	assert.Equal(t, "file", gotPath)
	assert.Equal(t, planted, secret.Reveal(v))
	assert.Equal(t, make([]byte, len(planted)), buf)
}

func TestValueReadReturnsTheErrorUnchanged(t *testing.T) {
	t.Parallel()
	want := errors.New("no such file")
	buf := []byte(planted)
	v, err := secret.Read(func(string) ([]byte, error) { return buf, want }, "file")
	assert.Same(t, want, err)
	assert.True(t, v.IsZero())
	assert.Equal(t, make([]byte, len(planted)), buf)
}
