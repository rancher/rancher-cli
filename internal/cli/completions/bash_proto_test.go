//go:build !windows

package completions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// bashProtoArgv runs the bash script with the given COMP_WORDS and COMP_CWORD
// and returns the argv vector the recorder binary received (excluding the binary
// name itself). Skips if bash is not on PATH (fails instead when CI is set).
func bashProtoArgv(t *testing.T, compWords []string, compCword int) []string {
	t.Helper()
	bash := requireShell(t, "bash")

	scriptPath := filepath.Join(t.TempDir(), "bash.sh")
	if err := os.WriteFile(scriptPath, bashScript, 0o644); err != nil {
		t.Fatalf("writing bash.sh: %v", err)
	}
	binDir := t.TempDir()
	argvFile := filepath.Join(binDir, "argv")
	fake := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\necho pod\n", argvFile)
	testfs.WriteExecutable(t, filepath.Join(binDir, "rancher"), []byte(fake))

	// Build COMP_WORDS array literal for bash.
	quoted := make([]string, len(compWords))
	for i, w := range compWords {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", "'\\''") + "'"
	}
	wordsLiteral := "(" + strings.Join(quoted, " ") + ")"

	// Stub faithfully models the -n "=:" reconstruction: single word-break chars
	// (= or :) that appear as standalone tokens are merged with their neighbours
	// so cur/words/cword reflect the collapsed form (e.g. --by=n, not n).
	driver := fmt.Sprintf(`
set +e
export PATH=%q:"$PATH"
_get_comp_words_by_ref() {
  local _nb=""
  while [[ $# -gt 0 ]]; do
    case "$1" in -n) _nb="$2"; shift 2 ;; *) shift ;; esac
  done
  words=("${COMP_WORDS[@]}")
  cword="${COMP_CWORD}"
  if [[ -n "$_nb" ]]; then
    local -a _w=()
    local _i _tok _lc _nc=-1
    for (( _i=0; _i<${#COMP_WORDS[@]}; _i++ )); do
      _tok="${COMP_WORDS[$_i]}"
      if [[ ${#_tok} -eq 1 && "$_nb" == *"$_tok"* && ${#_w[@]} -gt 0 ]]; then
        _w[${#_w[@]}-1]+="$_tok"
      elif [[ ${#_w[@]} -gt 0 ]]; then
        _lc="${_w[${#_w[@]}-1]: -1}"
        if [[ -n "$_lc" && "$_nb" == *"$_lc"* ]]; then
          _w[${#_w[@]}-1]+="$_tok"
        else
          _w+=("$_tok")
        fi
      else
        _w+=("$_tok")
      fi
      if [[ $_i -eq $COMP_CWORD ]]; then _nc=$(( ${#_w[@]} - 1 )); fi
    done
    words=("${_w[@]}")
    cword="$_nc"
  fi
  cur="${words[$cword]}"
  prev="${words[$((cword-1))]}"
}
source %q
COMP_WORDS=%s
COMP_CWORD=%d
COMP_TYPE=""
__rancher_bash_autocomplete
`, binDir, scriptPath, wordsLiteral, compCword)

	if out, err := exec.Command(bash, "-c", driver).CombinedOutput(); err != nil {
		t.Fatalf("bash driver: %v\n%s", err, out)
	}

	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("argv file not written (fake rancher never invoked?): %v", err)
	}
	return splitLines(strings.TrimRight(string(data), "\n"))
}

// TestBashRequestShape verifies that the bash script constructs the
// correct argv for the binary: proto sentinel, words before cursor, then the
// --rancher-completion-cur= field, then --generate-shell-completion.
func TestBashRequestShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		words    []string // COMP_WORDS
		cword    int      // COMP_CWORD
		wantArgs []string // argv after binary name
	}{
		{
			// cursor-after-space: empty current word → --rancher-completion-cur= (empty value).
			name:  "cursor-after-space empty cur",
			words: []string{"rancher", "get", "pod", ""},
			cword: 3,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "pod",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
		{
			// hostile: current word contains spaces → encoded as one self-prefixed field.
			name:  "hostile cur with spaces",
			words: []string{"rancher", "get", "a b c", ""},
			cword: 2,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get",
				"--rancher-completion-cur=a b c",
				"--generate-shell-completion",
			},
		},
		{
			// hostile: current word is a glob → encoded literally.
			name:  "hostile cur glob",
			words: []string{"rancher", "get", "p*"},
			cword: 2,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get",
				"--rancher-completion-cur=p*",
				"--generate-shell-completion",
			},
		},
		{
			// both-sides A: flag last in argv (no value yet), partial current word
			// → argv=[get, --cluster], cur=c- → SiteFlagValue(cluster, partial=c-).
			name:  "ambiguity side A: flag-last, partial cur → flag-value site",
			words: []string{"rancher", "get", "pod", "--cluster", "c-"},
			cword: 4,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "pod", "--cluster",
				"--rancher-completion-cur=c-",
				"--generate-shell-completion",
			},
		},
		{
			// both-sides B: flag+value consumed in argv, empty current word
			// → argv=[get, pod, --cluster, c-test], cur="" → SitePositional.
			name:  "ambiguity side B: flag+value consumed, empty cur → positional site",
			words: []string{"rancher", "get", "pod", "--cluster", "c-test", ""},
			cword: 5,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "pod", "--cluster", "c-test",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
		{
			// equals-form regression: bash's default word breaks split
			// --by=n into three tokens (--by, =, n) with COMP_CWORD pointing at n.
			// _get_comp_words_by_ref -n "=:" rejoins them; the request must carry
			// --rancher-completion-cur=--by=n, not --rancher-completion-cur=n.
			name:  "equals-form: --by=n reconstructed from bash word-splits",
			words: []string{"rancher", "get", "--by", "=", "n"},
			cword: 4,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get",
				"--rancher-completion-cur=--by=n",
				"--generate-shell-completion",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := bashProtoArgv(t, tc.words, tc.cword)
			if !sliceEqual(got, tc.wantArgs) {
				t.Errorf("argv mismatch\n got:  %q\n want: %q", got, tc.wantArgs)
			}
		})
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
