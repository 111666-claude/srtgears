/*
Tests for the search and stats integration in the Executor, which is shared by
the command line tool and the web interface.
*/
package exec

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icza/srtgears"
)

const fixture = "../testdata/search.srt"

func newExecutor(t *testing.T, args ...string) (*Executor, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	e := New(&buf)
	e.FlagSet.Usage = func() {}
	if err := e.ProcFlags(args); err != nil {
		t.Fatalf("ProcFlags: %v", err)
	}
	return e, &buf
}

func loadFixture(t *testing.T, e *Executor) {
	t.Helper()
	sp, err := srtgears.ReadSrtFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	e.Sp1 = sp
}

func runSearch(t *testing.T, args ...string) string {
	t.Helper()
	e, buf := newExecutor(t, append([]string{"-debug=false"}, args...)...)
	loadFixture(t, e)
	if err := e.GearIt(); err != nil {
		t.Fatalf("GearIt: %v", err)
	}
	return buf.String()
}

func TestExecutorSearchOutput(t *testing.T) {
	out := runSearch(t, "-in", fixture, "-search=force be")

	want := []string{
		`SEARCH of ` + fixture + ` for "force be":`,
		"#7 00:00:19,000 --> 00:00:21,000 (line 1, rune 9): Force / be",
		"#9 00:00:25,000 --> 00:00:27,000 (line 1, rune 9): Force be",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output does not contain %q\n%s", w, out)
		}
	}
	// Subtitle order is preserved: #7 before #9.
	if strings.Index(out, "#7 ") > strings.Index(out, "#9 ") {
		t.Error("hits are not in subtitle order")
	}
}

func TestExecutorSearchDuplicateAndCJK(t *testing.T) {
	out := runSearch(t, "-in", fixture, "-search=hello world")
	if strings.Count(out, "Hello, world") != 2 {
		t.Errorf("expected both duplicate lines, got:\n%s", out)
	}

	out = runSearch(t, "-in", fixture, "-search=你好世界")
	if !strings.Contains(out, "你好，世界") {
		t.Errorf("CJK hit missing:\n%s", out)
	}
}

func TestExecutorSearchEmptyQueryIsAnError(t *testing.T) {
	e, _ := newExecutor(t, "-debug=false", "-in", fixture, "-search=!!!")
	loadFixture(t, e)

	err := e.GearIt()
	if err == nil {
		t.Fatal("expected an error for an empty query")
	}
	if !strings.Contains(err.Error(), "no searchable characters") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExecutorSearchDoesNotRequireOut(t *testing.T) {
	// Search combined with a transformation must not demand an output file,
	// and must not create one.
	dir := t.TempDir()
	out := filepath.Join(dir, "out.srt")

	e, buf := newExecutor(t, "-debug=false", "-in", fixture, "-shiftBy=1500", "-search=hello")
	loadFixture(t, e)
	if err := e.GearIt(); err != nil {
		t.Fatalf("GearIt: %v", err)
	}
	// Transformed timestamps are reported.
	if !strings.Contains(buf.String(), "00:00:11,500") {
		t.Errorf("search did not report transformed time:\n%s", buf.String())
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unexpected output file: %v", err)
	}
}

func TestExecutorSearchMaxAndTruncation(t *testing.T) {
	out := runSearch(t, "-in", fixture, "-search=hello world", "-searchmax=1")
	if !strings.Contains(out, "Results truncated") {
		t.Errorf("expected truncation notice:\n%s", out)
	}
	if strings.Count(out, "Hello, world") != 1 {
		t.Errorf("expected exactly one hit:\n%s", out)
	}
}

func TestExecutorSearchNoMatches(t *testing.T) {
	out := runSearch(t, "-in", fixture, "-search=xyzzy")
	if !strings.Contains(out, "No matches found.") {
		t.Errorf("expected no-match notice:\n%s", out)
	}
}

// TestExecutorCLIAndWebParity verifies that the argument list produced by the
// web form and the arguments of the CLI produce identical, ordered output.
// The web's rewindForm appends the same "-search=..." and "-searchmax=..."
// arguments, so exercising ProcFlags covers both entry points.
func TestExecutorCLIAndWebParity(t *testing.T) {
	cli := runSearch(t, "-in", fixture, "-search=may the force")
	web := runSearch(t, "-debug=false", "-in", fixture, "-search=may the force", "-searchmax=0")
	if cli != web {
		t.Errorf("CLI and web output differ:\nCLI:\n%s\nweb:\n%s", cli, web)
	}

	// Hit order and positions match the engine's own results.
	sp, err := srtgears.ReadSrtFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sp.Search("may the force")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("got %d engine hits, want 2", len(res.Hits))
	}
	if res.Hits[0].TimeIn >= res.Hits[1].TimeIn {
		t.Error("engine hits are not ordered by time")
	}
}

func TestExecutorStatsUnchanged(t *testing.T) {
	// Stats keeps its own output and still marks the executor as stats-only.
	e, buf := newExecutor(t, "-debug=false", "-in", fixture, "-stats")
	loadFixture(t, e)
	if err := e.GearIt(); err != nil {
		t.Fatalf("GearIt: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "STATS of ") {
		t.Errorf("stats output changed:\n%s", buf.String())
	}
	if e.Modified {
		t.Error("stats should not mark the executor as modified")
	}
}
