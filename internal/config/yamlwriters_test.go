package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/yamlwrite"
)

// yamlRefusedRunes are the 35 code points the YAML library refuses when they
// appear raw in the text it parses (it changes U+0085 into a space): DEL,
// U+0080 to U+009F, U+FFFE and U+FFFF.
var yamlRefusedRunes = func() []rune {
	rs := []rune{0x7f}
	for r := rune(0x80); r <= 0x9f; r++ {
		rs = append(rs, r)
	}
	return append(rs, 0xfffe, 0xffff)
}()

// yamlNeighbourRunes are code points beside the set that the library accepts.
var yamlNeighbourRunes = []rune{0x7e, 0xa0, 0xfffd, 0x4e16, 0x1f600}

// yamlFullMarker holds the 35 refused characters and the five neighbours.
func yamlFullMarker() string {
	return string(yamlRefusedRunes) + string(yamlNeighbourRunes)
}

var yamlFixedTime = time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)

func yamlJoin(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// yamlFiller sets every field of a value to a string that holds a marker, and
// records the Go path of every field it reaches.
type yamlFiller struct {
	t      *testing.T
	marker string
	fields map[string]bool
	marked int
}

func jsonName(sf reflect.StructField) string {
	name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
	return name
}

func (f *yamlFiller) fill(v reflect.Value, path string) {
	f.t.Helper()
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(yamlFixedTime))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			sf := v.Type().Field(i)
			if !sf.IsExported() || jsonName(sf) == "-" {
				continue
			}
			p := yamlJoin(path, sf.Name)
			require.NotEmpty(f.t, jsonName(sf), "the field %s has no json name; the fill cannot tell what is written", p)
			require.False(f.t, sf.Anonymous, "the field %s is embedded; the fill cannot reach it", p)
			f.fields[p] = true
			f.fill(v.Field(i), p)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		f.fill(v.Elem(), path)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		f.fill(s.Index(0), path+"[]")
		v.Set(s)
	case reflect.Map:
		require.Equal(f.t, reflect.String, v.Type().Key().Kind(), "the map at %q has a key the fill cannot reach", path)
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		k.SetString(path + "{key}" + f.marker)
		f.marked++
		e := reflect.New(v.Type().Elem()).Elem()
		f.fill(e, path+"{}")
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString(path + f.marker)
		f.marked++
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Bool:
		v.SetBool(true)
	default:
		require.Failf(f.t, "a field the fill cannot reach", "the field %q has kind %s; extend the fill for it", path, v.Kind())
	}
}

// yamlWalkStrings calls visit with the Go path of every string field, string
// element and string map key of v.
func yamlWalkStrings(v reflect.Value, path string, visit func(path, s string)) {
	if v.Type() == reflect.TypeFor[time.Time]() {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			sf := v.Type().Field(i)
			if !sf.IsExported() || jsonName(sf) == "-" {
				continue
			}
			yamlWalkStrings(v.Field(i), yamlJoin(path, sf.Name), visit)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			yamlWalkStrings(v.Elem(), path, visit)
		}
	case reflect.Slice:
		for i := range v.Len() {
			yamlWalkStrings(v.Index(i), path+"[]", visit)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			visit(path+"{key}", k.String())
			yamlWalkStrings(v.MapIndex(k), path+"{}", visit)
		}
	case reflect.String:
		visit(path, v.String())
	}
}

// yamlTagNames collects the json name of every struct field reachable from t.
func yamlTagNames(t reflect.Type, into map[string]bool) {
	if t == reflect.TypeFor[time.Time]() {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		for sf := range t.Fields() {
			if !sf.IsExported() || jsonName(sf) == "-" {
				continue
			}
			into[jsonName(sf)] = true
			yamlTagNames(sf.Type, into)
		}
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		yamlTagNames(t.Elem(), into)
	}
}

// yamlWalkJSON asserts every string value and every object key that is not a
// json name of the type holds the marker, except the values at the exempt
// paths, and records each path it reaches.
func yamlWalkJSON(t *testing.T, node any, path string, tags map[string]bool, marker string, exempt map[string]string, visited map[string]bool) {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		for k, val := range n {
			child := yamlJoin(path, k)
			if !tags[k] {
				assert.Contains(t, k, marker, "the map key at %q in the json form", path)
				child = yamlJoin(path, "{}")
			}
			yamlWalkJSON(t, val, child, tags, marker, exempt, visited)
		}
	case []any:
		for _, e := range n {
			yamlWalkJSON(t, e, path+"[]", tags, marker, exempt, visited)
		}
	case string:
		visited[path] = true
		if _, ok := exempt[path]; !ok {
			assert.Contains(t, n, marker, "the string at %q in the json form", path)
		}
	}
}

// yamlWriter describes one writer and its reader for the reflection test.
type yamlWriter[T any] struct {
	name string
	// exempt lists the Go paths of fields the reader requires in a fixed
	// form, each with the reason; jsonExempt lists the same fields by their
	// json path.
	exempt     map[string]string
	jsonExempt map[string]string
	// fix sets the exempt fields to a valid form.
	fix func(*T)
	// refuse runs the reader's validation on a value and returns its error;
	// a writer with exempt fields has one.
	refuse func(dir string, v *T) error
	file   func(dir string, v *T) string
	write  func(dir string, v *T) error
	read   func(t *testing.T, dir string, v *T) *T
}

func yamlJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// roundTrip fills a value with the marker, writes it with the real writer,
// reads it with the real reader and compares the decoded values.
func (w yamlWriter[T]) roundTrip(t *testing.T, marker string, ordinary bool) (marked int) {
	t.Helper()
	var v T
	filler := &yamlFiller{t: t, marker: marker, fields: map[string]bool{}}
	rv := reflect.ValueOf(&v).Elem()
	filler.fill(rv, "")
	w.fix(&v)

	for p := range w.exempt {
		assert.True(t, filler.fields[p], "the exempt table of %s names the field %q, which the type does not have", w.name, p)
	}
	yamlWalkStrings(rv, "", func(path, s string) {
		if _, ok := w.exempt[path]; !ok {
			assert.Contains(t, s, marker, "%s: the field %q does not hold the marker", w.name, path)
		}
	})
	tags := map[string]bool{}
	yamlTagNames(rv.Type(), tags)
	var decoded any
	require.NoError(t, json.Unmarshal([]byte(yamlJSON(t, v)), &decoded))
	visited := map[string]bool{}
	yamlWalkJSON(t, decoded, "", tags, marker, w.jsonExempt, visited)
	for p := range w.jsonExempt {
		assert.True(t, visited[p], "the json exempt table of %s names the path %q, which the value does not have", w.name, p)
	}

	dir := t.TempDir()
	require.NoError(t, w.write(dir, &v), "%s: the write", w.name)
	raw, err := os.ReadFile(w.file(dir, &v))
	require.NoError(t, err)
	for _, r := range yamlRefusedRunes {
		assert.False(t, bytes.ContainsRune(raw, r), "%s: %U is written raw", w.name, r)
	}
	if ordinary {
		want, err := sigsyaml.Marshal(&v)
		require.NoError(t, err)
		assert.Equal(t, want, raw, "%s: the bytes differ from the library's", w.name)
	}
	got := w.read(t, dir, &v)
	assert.JSONEq(t, yamlJSON(t, v), yamlJSON(t, *got), "%s: the value read back", w.name)
	return filler.marked
}

// yamlSetField sets the field at path (the notation of the exempt tables) to a
// form the reader cannot accept: a string field holds the path and the
// marker, an integer field a value no reader accepts.
func yamlSetField(t *testing.T, v reflect.Value, path, marker string) {
	t.Helper()
	cur := v
	for seg := range strings.SplitSeq(path, ".") {
		name, indexed := strings.CutSuffix(seg, "[]")
		cur = reflect.Indirect(cur).FieldByName(name)
		require.True(t, cur.IsValid(), "the type has no field %q of %q", name, path)
		if indexed {
			require.Equal(t, reflect.Slice, cur.Kind(), "%q", path)
			require.NotZero(t, cur.Len(), "%q", path)
			cur = cur.Index(0)
		}
	}
	cur = reflect.Indirect(cur)
	switch cur.Kind() {
	case reflect.String:
		cur.SetString(path + marker)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		cur.SetInt(99)
	default:
		require.Failf(t, "an exempt field of a kind the test cannot set", "%q has kind %s", path, cur.Kind())
	}
}

// exemptionsAreNeeded asserts each exempt field is refused by the reader's
// validation when it holds the marker, with every other field valid: the
// exemption has a reason. A value with every exempt field valid is accepted.
func (w yamlWriter[T]) exemptionsAreNeeded(t *testing.T) {
	t.Helper()
	if len(w.exempt) == 0 {
		assert.Nil(t, w.refuse, "%s has no exempt field and a refusal check", w.name)
		return
	}
	require.NotNil(t, w.refuse, "%s has exempt fields and no check that the reader refuses them", w.name)
	build := func() T {
		var v T
		filler := &yamlFiller{t: t, marker: "m", fields: map[string]bool{}}
		filler.fill(reflect.ValueOf(&v).Elem(), "")
		w.fix(&v)
		return v
	}
	valid := build()
	require.NoError(t, w.refuse(t.TempDir(), &valid), "%s: a value with every exempt field valid is accepted", w.name)
	for _, p := range slices.Sorted(maps.Keys(w.exempt)) {
		t.Run(p, func(t *testing.T) {
			v := build()
			yamlSetField(t, reflect.ValueOf(&v).Elem(), p, yamlFullMarker())
			assert.Error(t, w.refuse(t.TempDir(), &v), "%s: the reader accepts %s holding the marker; the exemption has no reason", w.name, p)
		})
	}
}

// run writes and reads the full marker, each of the 35 characters alone and
// an ordinary value.
func (w yamlWriter[T]) run(t *testing.T) {
	t.Helper()
	t.Run("full marker", func(t *testing.T) {
		marked := w.roundTrip(t, yamlFullMarker(), false)
		t.Logf("%s: the fill marked %d strings", w.name, marked)
	})
	for _, r := range yamlRefusedRunes {
		t.Run(fmt.Sprintf("%U alone", r), func(t *testing.T) {
			w.roundTrip(t, string(r), false)
		})
	}
	t.Run("ordinary value", func(t *testing.T) {
		w.roundTrip(t, "plain", true)
	})
	t.Run("exemptions are needed", w.exemptionsAreNeeded)
}

func yamlHex(n int) string { return strings.Repeat("a", n) }

// TestYAMLWritersReadBackEveryRefusedCharacter asserts each writer type, with
// every string field and map key filled with the 35 refused characters and
// their neighbours (and again with each of the 35 alone), is written by the
// real writer with none of the 35 raw in the file and read back by the real
// reader as the same value, and that an ordinary value is written with the
// bytes sigs.k8s.io/yaml writes. A field the reader requires in a fixed form is
// exempt from the marker, and each exemption is asserted to be needed.
func TestYAMLWritersReadBackEveryRefusedCharacter(t *testing.T) {
	t.Run("config.yaml", func(t *testing.T) {
		yamlWriter[Config]{
			name: "Config.Write",
			fix:  func(*Config) {},
			file: func(dir string, _ *Config) string { return FilePath(dir) },
			write: func(dir string, v *Config) error {
				v.SetPath(FilePath(dir))
				in := v.recordInputs()
				v.loadedInputs = &in
				return v.Write()
			},
			read: func(t *testing.T, dir string, _ *Config) *Config {
				t.Helper()
				c, err := Load(FilePath(dir))
				require.NoError(t, err)
				return c
			},
		}.run(t)
	})
	t.Run("credentials.yaml", func(t *testing.T) {
		yamlWriter[map[string]string]{
			name: "fileStore.write",
			fix:  func(*map[string]string) {},
			file: func(dir string, _ *map[string]string) string { return CredentialsFilePath(dir) },
			write: func(dir string, v *map[string]string) error {
				return newFileStore(dir, &noticeBuffer{}).write(*v)
			},
			read: func(t *testing.T, dir string, _ *map[string]string) *map[string]string {
				t.Helper()
				m, err := newFileStore(dir, &noticeBuffer{}).read()
				require.NoError(t, err)
				for k, val := range m {
					assert.Equal(t, val, ReadFileTokenSilent(dir, k), "ReadFileTokenSilent")
				}
				return &m
			},
		}.run(t)
	})
	t.Run("token-cache.yaml", func(t *testing.T) {
		yamlWriter[map[string]cachedCred]{
			name:       "fileStore.writeCache",
			jsonExempt: map[string]string{"{}.expiry": "the fixed time the fill sets"},
			fix:        func(*map[string]cachedCred) {},
			file: func(dir string, _ *map[string]cachedCred) string {
				return filepath.Join(dir, "token-cache.yaml")
			},
			write: func(dir string, v *map[string]cachedCred) error {
				return newFileStore(dir, &noticeBuffer{}).writeCache(*v)
			},
			read: func(t *testing.T, dir string, _ *map[string]cachedCred) *map[string]cachedCred {
				t.Helper()
				m, err := newFileStore(dir, &noticeBuffer{}).readCache()
				require.NoError(t, err)
				return &m
			},
		}.run(t)
	})
	t.Run("credential identity record", func(t *testing.T) {
		yamlWriter[identityRecord]{
			name: "writeIdentityRecord",
			exempt: map[string]string{
				"Version":           "the reader accepts one version",
				"State":             "the reader accepts two states",
				"ID":                "the reader requires lower-case hex of a fixed length",
				"Fingerprint":       "the reader requires lower-case hex of a fixed length",
				"Prior.ID":          "the reader requires lower-case hex of a fixed length",
				"Prior.Fingerprint": "the reader requires lower-case hex of a fixed length",
			},
			jsonExempt: map[string]string{
				"state":             "the reader accepts two states",
				"id":                "the reader requires lower-case hex of a fixed length",
				"fingerprint":       "the reader requires lower-case hex of a fixed length",
				"prior.id":          "the reader requires lower-case hex of a fixed length",
				"prior.fingerprint": "the reader requires lower-case hex of a fixed length",
			},
			refuse: func(dir string, v *identityRecord) error {
				data, err := yamlwrite.Marshal(*v)
				if err != nil {
					return err
				}
				_, err = parseIdentityRecord(dir, v.Entry, data)
				return err
			},
			fix: func(r *identityRecord) {
				r.Version = identityVersion
				r.State = identityPending
				r.ID = yamlHex(identityIDLen)
				r.Fingerprint = yamlHex(identityFingerprintLen)
				r.Prior.ID = yamlHex(identityIDLen)
				r.Prior.Fingerprint = yamlHex(identityFingerprintLen)
			},
			file: func(dir string, v *identityRecord) string {
				return filepath.Join(identityDirPath(dir), identityFileName(v.Entry))
			},
			write: func(dir string, v *identityRecord) error { return writeIdentityRecord(dir, *v) },
			read: func(t *testing.T, dir string, v *identityRecord) *identityRecord {
				t.Helper()
				r, err := readIdentityRecord(dir, v.Entry)
				require.NoError(t, err)
				require.NotNil(t, r)
				return r
			},
		}.run(t)
	})
	t.Run("credential store change record", func(t *testing.T) {
		yamlWriter[transitionManifest]{
			name: "writeTransition",
			exempt: map[string]string{
				"Version":                  "the reader accepts one version",
				"ID":                       "the reader requires lower-case hex of a fixed length",
				"Steps[].Kind":             "the reader accepts two kinds",
				"Steps[].Source":           "the reader accepts the file backend, the keychain or a helper",
				"Steps[].VerificationID":   "the reader requires lower-case hex of a fixed length",
				"Steps[].CredentialDigest": "the reader requires lower-case hex of a fixed length",
			},
			jsonExempt: map[string]string{
				"id":                       "the reader requires lower-case hex of a fixed length",
				"steps[].kind":             "the reader accepts two kinds",
				"steps[].source":           "the reader accepts the file backend, the keychain or a helper",
				"steps[].verificationId":   "the reader requires lower-case hex of a fixed length",
				"steps[].credentialDigest": "the reader requires lower-case hex of a fixed length",
			},
			refuse: func(dir string, v *transitionManifest) error { return validateTransition(dir, v) },
			fix: func(m *transitionManifest) {
				m.Version = transitionVersion
				m.ID = yamlHex(transitionIDLen)
				m.Steps[0].Kind = transitionDelete
				m.Steps[0].Source = backendFile
				m.Steps[0].VerificationID = yamlHex(transitionIDLen)
				m.Steps[0].CredentialDigest = yamlHex(identityFingerprintLen)
			},
			file:  func(dir string, _ *transitionManifest) string { return TransitionPath(dir) },
			write: func(dir string, v *transitionManifest) error { return writeTransition(dir, *v) },
			read: func(t *testing.T, dir string, _ *transitionManifest) *transitionManifest {
				t.Helper()
				m, err := readTransition(dir)
				require.NoError(t, err)
				require.NotNil(t, m)
				return m
			},
		}.run(t)
	})
	t.Run("credential helper records", func(t *testing.T) {
		yamlWriter[helperRecords]{
			name:       "persistHelperRecordsToDisk",
			exempt:     map[string]string{"Entries[].Helper": "the reader requires a helper name of lower-case letters, digits and hyphens"},
			jsonExempt: map[string]string{"entries[].helper": "the reader requires a helper name of lower-case letters, digits and hyphens"},
			fix:        func(s *helperRecords) { s.Entries[0].Helper = "helper-one" },
			refuse: func(dir string, v *helperRecords) error {
				if err := persistHelperRecordsToDisk(dir, v); err != nil {
					return err
				}
				_, err := readHelperRecords(dir)
				return err
			},
			file:  func(dir string, _ *helperRecords) string { return filepath.Join(dir, helperRecordsFileName) },
			write: func(dir string, v *helperRecords) error { return persistHelperRecordsToDisk(dir, v) },
			read: func(t *testing.T, dir string, _ *helperRecords) *helperRecords {
				t.Helper()
				s, err := readHelperRecords(dir)
				require.NoError(t, err)
				return s
			},
		}.run(t)
	})
}

// yamlRow writes a minimal valid value with the full marker in one field,
// reads it back and asserts the field.
type yamlRow struct {
	writer, field string
	run           func(t *testing.T, marker string)
}

func yamlConfigRow(field string, set func(c *Config, marker string), get func(c *Config) string) yamlRow {
	return yamlRow{"Config.Write", field, func(t *testing.T, marker string) {
		t.Helper()
		dir := t.TempDir()
		c := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
		set(c, marker)
		c.SetPath(FilePath(dir))
		require.NoError(t, c.Write())
		got, err := Load(FilePath(dir))
		require.NoError(t, err)
		assert.Equal(t, marker, get(got))
	}}
}

func yamlOnlyServer(c *Config) Server {
	for _, s := range c.Servers {
		return s
	}
	return Server{}
}

func yamlStepRow(field string, set func(s *transitionStep, marker string), get func(s transitionStep) string) yamlRow {
	return yamlRow{"writeTransition", field, func(t *testing.T, marker string) {
		t.Helper()
		dir := t.TempDir()
		step := transitionStep{
			Kind: transitionDelete, Entry: "e", Source: backendFile,
			VerificationID: yamlHex(transitionIDLen), CredentialDigest: yamlHex(identityFingerprintLen),
		}
		set(&step, marker)
		require.NoError(t, writeTransition(dir, transitionManifest{Version: transitionVersion, ID: yamlHex(transitionIDLen), Steps: []transitionStep{step}}))
		m, err := readTransition(dir)
		require.NoError(t, err)
		require.Len(t, m.Steps, 1)
		assert.Equal(t, marker, get(m.Steps[0]))
	}}
}

func yamlCacheRow(field string, set func(c *cachedCred, key *string, marker string), get func(key string, c cachedCred) string) yamlRow {
	return yamlRow{"fileStore.writeCache", field, func(t *testing.T, marker string) {
		t.Helper()
		dir := t.TempDir()
		key := "k"
		c := cachedCred{Token: "t", Expiry: yamlFixedTime}
		set(&c, &key, marker)
		f := newFileStore(dir, &noticeBuffer{})
		require.NoError(t, f.writeCache(map[string]cachedCred{key: c}))
		m, err := f.readCache()
		require.NoError(t, err)
		require.Len(t, m, 1)
		for k, got := range m {
			assert.Equal(t, marker, get(k, got))
		}
	}}
}

func yamlHelperRow(field string, set func(r *HelperRecord, marker string), get func(r HelperRecord) string) yamlRow {
	return yamlRow{"persistHelperRecordsToDisk", field, func(t *testing.T, marker string) {
		t.Helper()
		dir := t.TempDir()
		rec := HelperRecord{Helper: "helper-one", Name: "n", CanonicalURL: "https://r.ex"}
		set(&rec, marker)
		require.NoError(t, persistHelperRecordsToDisk(dir, &helperRecords{Entries: []HelperRecord{rec}}))
		s, err := readHelperRecords(dir)
		require.NoError(t, err)
		require.Len(t, s.Entries, 1)
		assert.Equal(t, marker, get(s.Entries[0]))
	}}
}

// TestYAMLWritersReadBackNamedFields asserts each field that can hold a
// refused character comes back exactly from the file its writer wrote.
func TestYAMLWritersReadBackNamedFields(t *testing.T) {
	setServer := func(c *Config, change func(s *Server)) {
		s := c.Servers["prod"]
		change(&s)
		c.Servers["prod"] = s
	}
	rows := []yamlRow{
		yamlConfigRow("servers key", func(c *Config, m string) {
			c.Servers = map[string]Server{m: {URL: "https://r.ex"}}
		}, func(c *Config) string {
			for k := range c.Servers {
				return k
			}
			return ""
		}),
		yamlConfigRow("currentServer", func(c *Config, m string) { c.CurrentServer = m }, func(c *Config) string { return c.CurrentServer }),
		yamlConfigRow("servers.<n>.url", func(c *Config, m string) { setServer(c, func(s *Server) { s.URL = m }) }, func(c *Config) string { return yamlOnlyServer(c).URL }),
		yamlConfigRow("servers.<n>.tokenAuthConfig", func(c *Config, m string) { setServer(c, func(s *Server) { s.TokenAuthConfig = m }) }, func(c *Config) string { return yamlOnlyServer(c).TokenAuthConfig }),
		yamlConfigRow("servers.<n>.tokenUserId", func(c *Config, m string) { setServer(c, func(s *Server) { s.TokenUserID = m }) }, func(c *Config) string { return yamlOnlyServer(c).TokenUserID }),
		yamlConfigRow("servers.<n>.tokenRef.name", func(c *Config, m string) {
			setServer(c, func(s *Server) { s.TokenRef = &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: m} })
		}, func(c *Config) string { return yamlOnlyServer(c).TokenRef.Name }),
		yamlConfigRow("servers.<n>.credentialExpiresAt", func(c *Config, m string) { setServer(c, func(s *Server) { s.CredentialExpiresAt = m }) }, func(c *Config) string { return yamlOnlyServer(c).CredentialExpiresAt }),
		yamlConfigRow("servers.<n>.supersededTokens[].lastAttempt.error", func(c *Config, m string) {
			setServer(c, func(s *Server) {
				s.SupersededTokens = []SupersededToken{{
					Ref:          clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "old"},
					SupersededAt: "2030-01-02T03:04:05Z",
					LastAttempt:  &clierr.RevokeOutcome{Result: "failed", Error: m},
				}}
			})
		}, func(c *Config) string { return yamlOnlyServer(c).SupersededTokens[0].LastAttempt.Error }),

		{"fileStore.write", "token (value)", func(t *testing.T, marker string) {
			t.Helper()
			dir := t.TempDir()
			f := newFileStore(dir, &noticeBuffer{})
			require.NoError(t, f.write(map[string]string{"prod": marker}))
			m, err := f.read()
			require.NoError(t, err)
			assert.Equal(t, marker, m["prod"])
			assert.Equal(t, marker, ReadFileTokenSilent(dir, "prod"))
		}},
		{"fileStore.write", "entry name (key)", func(t *testing.T, marker string) {
			t.Helper()
			dir := t.TempDir()
			f := newFileStore(dir, &noticeBuffer{})
			require.NoError(t, f.write(map[string]string{marker: "tok"}))
			m, err := f.read()
			require.NoError(t, err)
			assert.Equal(t, map[string]string{marker: "tok"}, m)
			assert.Equal(t, "tok", ReadFileTokenSilent(dir, marker))
		}},

		yamlCacheRow("token", func(c *cachedCred, _ *string, m string) { c.Token = m }, func(_ string, c cachedCred) string { return c.Token }),
		yamlCacheRow("key", func(_ *cachedCred, k *string, m string) { *k = m }, func(k string, _ cachedCred) string { return k }),
		yamlCacheRow("name", func(c *cachedCred, _ *string, m string) { c.Name = m }, func(_ string, c cachedCred) string { return c.Name }),
		yamlCacheRow("server", func(c *cachedCred, _ *string, m string) { c.Server = m }, func(_ string, c cachedCred) string { return c.Server }),
		yamlCacheRow("cluster", func(c *cachedCred, _ *string, m string) { c.Cluster = m }, func(_ string, c cachedCred) string { return c.Cluster }),
		yamlCacheRow("authConfig", func(c *cachedCred, _ *string, m string) { c.AuthConfig = m }, func(_ string, c cachedCred) string { return c.AuthConfig }),
		yamlCacheRow("entry", func(c *cachedCred, _ *string, m string) { c.Entry = m }, func(_ string, c cachedCred) string { return c.Entry }),
		yamlCacheRow("identified", func(c *cachedCred, _ *string, m string) { c.Identified = m }, func(_ string, c cachedCred) string { return c.Identified }),
		yamlCacheRow("tokenRef.name", func(c *cachedCred, _ *string, m string) {
			c.TokenRef = &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: m}
		}, func(_ string, c cachedCred) string { return c.TokenRef.Name }),

		{"writeIdentityRecord", "entry", func(t *testing.T, marker string) {
			t.Helper()
			dir := t.TempDir()
			rec := identityRecord{
				Version: identityVersion, Entry: marker, State: identityActive,
				ID: yamlHex(identityIDLen), Fingerprint: yamlHex(identityFingerprintLen),
			}
			require.NoError(t, writeIdentityRecord(dir, rec))
			got, err := readIdentityRecord(dir, marker)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, marker, got.Entry)
		}},

		yamlStepRow("step entry", func(s *transitionStep, m string) { s.Entry = m }, func(s transitionStep) string { return s.Entry }),
		yamlStepRow("lastError", func(s *transitionStep, m string) { s.LastError = m }, func(s transitionStep) string { return s.LastError }),
		yamlStepRow("canonicalUrl", func(s *transitionStep, m string) { s.CanonicalURL = m }, func(s transitionStep) string { return s.CanonicalURL }),

		yamlHelperRow("name", func(r *HelperRecord, m string) { r.Name = m }, func(r HelperRecord) string { return r.Name }),
		yamlHelperRow("canonicalURL", func(r *HelperRecord, m string) { r.CanonicalURL = m }, func(r HelperRecord) string { return r.CanonicalURL }),
	}
	for _, row := range rows {
		t.Run(row.writer+" "+row.field, func(t *testing.T) {
			row.run(t, yamlFullMarker())
		})
	}
}

// TestConfigWriteLeavesTheFileAloneWhenAKeyIsTooLong asserts a server name
// whose escaped form passes the YAML library's key length limit makes the
// write fail and leaves an existing config.yaml as it was.
func TestConfigWriteLeavesTheFileAloneWhenAKeyIsTooLong(t *testing.T) {
	dir := t.TempDir()
	c := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	c.SetPath(FilePath(dir))
	require.NoError(t, c.Write())
	before, err := os.ReadFile(FilePath(dir))
	require.NoError(t, err)

	long := &Config{Servers: map[string]Server{strings.Repeat("\u007f", 171): {URL: "https://r.ex"}}}
	long.SetPath(FilePath(dir))
	require.Error(t, long.Write())

	after, err := os.ReadFile(FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// TestLoginWithRefusedCharactersIsStoredAndReadBack asserts a login under a
// name, or with a bearer, that holds characters the YAML library refuses
// writes the config, commits the credential and reads both back exactly.
func TestLoginWithRefusedCharactersIsStoredAndReadBack(t *testing.T) {
	for _, tc := range []struct {
		name, entry, bearer string
		owns                bool
	}{
		{"a new entry named with U+FFFF", "a\uffffb", "tok-one", true},
		{"a new entry named with U+FFFE", "a\ufffeb", "tok-two", true},
		{"an existing entry with DEL and U+0085 in the bearer", "prod", "tok\u007fa\u0085b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := settledFixture(t)
			in := settledInput()
			in.Name = tc.entry
			in.Bearer = tc.bearer
			in.OwnsEntry = tc.owns

			out, err := PersistLoginCredential(t.Context(), dir, in)
			require.NoError(t, err)
			assert.Equal(t, clierr.ConfigWritten, out.ConfigState)
			assert.Equal(t, clierr.CredentialCommitted, out.CredentialState)

			cfg, err := Load(FilePath(dir))
			require.NoError(t, err)
			names := make([]string, 0, len(cfg.Servers))
			for n := range cfg.Servers {
				names = append(names, n)
			}
			require.True(t, slices.Contains(names, tc.entry), "the config holds %q, not %q", names, tc.entry)

			tok, err := NewFileStore(dir).GetToken(ServerRef{Name: tc.entry, URL: "https://r.ex"})
			require.NoError(t, err)
			assert.Equal(t, tc.bearer, tok)
		})
	}
}
