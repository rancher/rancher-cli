package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerAccessDefaultsByCommandType asserts the value a descriptor that
// declares none gets: write for a mutating command, read for a read-only
// one, none for a local one; a declared value is kept.
func TestServerAccessDefaultsByCommandType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		d    Descriptor
		want ServerAccess
	}{
		{"mutating", Descriptor{CommandType: Mutating}, ServerAccessWrite},
		{"read-only", Descriptor{CommandType: ReadOnly}, ServerAccessRead},
		{"read-only declaring none", Descriptor{CommandType: ReadOnly, ServerAccess: ServerAccessNone}, ServerAccessNone},
		{"local", Descriptor{CommandType: Local}, ServerAccessNone},
		{"local declaring read", Descriptor{CommandType: Local, ServerAccess: ServerAccessRead}, ServerAccessRead},
		{"local declaring write", Descriptor{CommandType: Local, ServerAccess: ServerAccessWrite}, ServerAccessWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.d.EffectiveServerAccess())
		})
	}
}

// TestValidateServerAccessRules asserts the registration rules: a mutating
// command may only write, a read-only command may read or declare none, a
// local command may declare any of the three, and an unknown value is
// refused.
func TestValidateServerAccessRules(t *testing.T) {
	t.Parallel()
	binding := stubResolver{"m": {Contract: safety.ContractPublic, Stability: safety.Stable}}
	cases := []struct {
		name    string
		cmd     Command
		wantErr string
	}{
		{"mutating declaring write", fakeMutating{d: Descriptor{Name: "m", CommandType: Mutating, Shape: ShapeText, ServerAccess: ServerAccessWrite}}, ""},
		{"mutating declaring read", fakeMutating{d: Descriptor{Name: "m", CommandType: Mutating, Shape: ShapeText, ServerAccess: ServerAccessRead}}, `command "m": a mutating command's serverAccess must be "write", not "read"`},
		{"mutating declaring none", fakeMutating{d: Descriptor{Name: "m", CommandType: Mutating, Shape: ShapeText, ServerAccess: ServerAccessNone}}, `a mutating command's serverAccess must be "write", not "none"`},
		{"read-only declaring none", fakeRead{d: Descriptor{Name: "r", CommandType: ReadOnly, Shape: ShapeText, ServerAccess: ServerAccessNone}}, ""},
		{"read-only declaring write", fakeRead{d: Descriptor{Name: "r", CommandType: ReadOnly, Shape: ShapeText, ServerAccess: ServerAccessWrite}}, `command "r": a read-only command's serverAccess must be "read" or "none", not "write"`},
		{"local declaring write", fakeRead{d: Descriptor{Name: "l", CommandType: Local, Shape: ShapeText, ServerAccess: ServerAccessWrite}}, ""},
		{"unknown value", fakeRead{d: Descriptor{Name: "l", CommandType: Local, Shape: ShapeText, ServerAccess: "network"}}, `command "l": unknown serverAccess "network"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := NewRegistry(binding)
			reg.Register(tc.cmd)
			err := reg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestSchemaCarriesServerAccess asserts the manifest field: always the
// value the command runs with, its default included.
func TestSchemaCarriesServerAccess(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{"create widget": {Contract: safety.ContractPublic, Stability: safety.Stable}})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "schema", CommandType: ReadOnly, ServerAccess: ServerAccessNone}})
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating}})
	reg.Register(fakeRead{d: Descriptor{Name: "config path", CommandType: Local}})
	reg.Register(fakeRead{d: Descriptor{Name: "auth logout", CommandType: Local, ServerAccess: ServerAccessWrite}})

	got := map[string]ServerAccess{}
	for _, c := range reg.Schema("v-test", "2.14").Commands {
		got[c.Name] = c.ServerAccess
	}
	assert.Equal(t, map[string]ServerAccess{
		"get widget":    ServerAccessRead,
		"schema":        ServerAccessNone,
		"create widget": ServerAccessWrite,
		"config path":   ServerAccessNone,
		"auth logout":   ServerAccessWrite,
	}, got)
}

// TestContractLineServerAccess asserts the contract line's server segment:
// server=read and server=write, and none for a command that never
// contacts the server.
func TestContractLineServerAccess(t *testing.T) {
	t.Parallel()
	cases := []struct {
		access ServerAccess
		want   string
	}{
		{ServerAccessNone, "contract: local · auth"},
		{ServerAccessRead, "contract: local · server=read · auth"},
		{ServerAccessWrite, "contract: local · server=write · auth"},
	}
	for _, tc := range cases {
		t.Run(string(tc.access), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, contractLine(SchemaCommand{CommandType: Local, ServerAccess: tc.access, Auth: true}))
		})
	}
}
