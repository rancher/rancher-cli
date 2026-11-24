package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fuzzWithin runs fn and fails the test when it does not return within a second.
func fuzzWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Sprintf("%v\n%s", r, debug.Stack())
				return
			}
			done <- nil
		}()
		fn()
	}()
	select {
	case r := <-done:
		if r != nil {
			panic(r)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s did not return within 1 second", what)
	}
}

// fuzzWriteCounter is a writer that keeps what is written to it.
type fuzzWriteCounter struct {
	mu   sync.Mutex
	data []byte
}

func (w *fuzzWriteCounter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *fuzzWriteCounter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.data)
}

// fuzzStdStreams replaces os.Stdout and os.Stderr with pipes that goroutines read, and returns
// the function that puts them back and returns what was written to each. The function runs once,
// however often it is called. The test's cleanup calls it: a panic or a failed assertion before
// the caller's call still puts the streams back.
func fuzzStdStreams(t *testing.T) (restore func() (stdout, stderr string)) {
	t.Helper()
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	var wg sync.WaitGroup
	var outText, errText []byte
	wg.Go(func() { outText, _ = io.ReadAll(outR) })
	wg.Go(func() { errText, _ = io.ReadAll(errR) })
	restore = sync.OnceValues(func() (string, string) {
		os.Stdout, os.Stderr = origOut, origErr
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
		return string(outText), string(errText)
	})
	t.Cleanup(func() { restore() })
	return restore
}

// FuzzParseLine fuzzes the command-line parser. A command line is one NUL-joined string and a
// presence flag, as FuzzParseArgTail encodes it. Whatever the arguments, parsing and the refusal
// check end, write nothing, give the same result a second time, and a flag error is a typed
// usage error that the dispatcher turns into a VALIDATION error without running anything. Never
// call t.Parallel() here: it panics inside f.Fuzz.
func FuzzParseLine(f *testing.F) {
	for _, words := range fuzzGoldenLines(f, true, 0, "parse", "candidates") {
		blob, has := fuzzJoin(words)
		f.Add(blob, has)
	}
	fuzzHermetic(f)
	fuzzNoStoredConfig(f)
	root, err := BuildRoot("v-fuzz")
	require.NoError(f, err)

	f.Fuzz(func(t *testing.T, argsBlob string, hasArgs bool) {
		if len(argsBlob) > 64<<10 {
			t.Skip("input over 64 KiB")
		}
		words := fuzzSplit(argsBlob, hasArgs)
		if fuzzHasConfigWord(words) {
			t.Skip("--config")
		}

		var writes fuzzWriteCounter
		root.Writer, root.ErrWriter, root.Reader = &writes, &writes, nil
		restore := fuzzStdStreams(t)
		var p, again parsedLine
		var refusal *clierr.Error
		fuzzWithin(t, "parseLine", func() { p = parseLine(root, words) })
		fuzzWithin(t, "lineRefusal", func() { refusal, _ = lineRefusal(p) })
		fuzzWithin(t, "parseLine of the same words", func() { again = parseLine(root, words) })
		stdout, stderr := restore()
		assert.Empty(t, stdout, "parsing wrote to os.Stdout")
		assert.Empty(t, stderr, "parsing wrote to os.Stderr")
		assert.Empty(t, writes.text(), "parsing wrote to a writer of the root")
		assert.Equal(t, p, again, "a second parse of the same words gave another answer")

		if refusal != nil {
			require.Equal(t, clierr.CodeValidation, refusal.Code, "a refusal that is not a VALIDATION error")
		}
		if p.end.kind != lineError {
			return
		}
		ue := p.levels[p.end.level].usageErr
		require.Contains(t, []lineErrorClass{undefinedFlag, missingFlagValue, repeatedFlag, badBoolValue}, ue.class,
			"a flag error without one of the four classes")
		CaptureArgv(words)
		var out, errOut fuzzWriteCounter
		root.Writer, root.ErrWriter = &out, &errOut
		oc := dispatch(t.Context(), root, &p)
		require.Equal(t, outcomeUsageError, oc.kind)
		ce, ok := clierrOf(oc.err)
		require.True(t, ok, "the error of a flag error is not a *clierr.Error: %v", oc.err)
		require.Equal(t, clierr.CodeValidation, ce.Code)
	})
}

// clierrOf returns the *clierr.Error err wraps.
func clierrOf(err error) (*clierr.Error, bool) {
	return errors.AsType[*clierr.Error](err)
}
