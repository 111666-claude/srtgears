/*
Tests for the subtitle search index and phrase search.

The fixture testdata/search.srt deliberately contains:

  - cross-line dialogue ("Senator, we're making\nour final approach ...",
    "The Force will be with you.\nAlways."),
  - duplicate dialogue ("Hello, world!" twice, and "May the Force be with you."
    twice in different forms),
  - mixed English and Chinese punctuation ("Hello, world!", "你好，世界！"),
  - HTML formatting (<i>May the Force</i>),
  - an apostrophe ("we're", "Don't").
*/
package srtgears

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func readSearchFixture(t *testing.T) *SubsPack {
	t.Helper()
	sp, err := ReadSrtFile("testdata/search.srt")
	if err != nil {
		t.Fatalf("ReadSrtFile: %v", err)
	}
	return sp
}

// checkHits verifies the hit subtitle indexes, times and matched text.
type wantHit struct {
	subIndex int
	timeIn   time.Duration
	matched  string
}

func checkHits(t *testing.T, res *SearchResults, wants []wantHit) {
	t.Helper()
	if len(res.Hits) != len(wants) {
		var got []string
		for _, h := range res.Hits {
			got = append(got, h.MatchedText())
		}
		t.Fatalf("got %d hits %q, want %d", len(res.Hits), got, len(wants))
	}
	for i, w := range wants {
		h := res.Hits[i]
		if h.SubIndex != w.subIndex {
			t.Errorf("hit %d: SubIndex = %d, want %d", i, h.SubIndex, w.subIndex)
		}
		if h.TimeIn != w.timeIn {
			t.Errorf("hit %d: TimeIn = %v, want %v", i, h.TimeIn, w.timeIn)
		}
		if got := h.MatchedText(); got != w.matched {
			t.Errorf("hit %d: MatchedText = %q, want %q", i, got, w.matched)
		}
	}
}

func TestSearchCasePunctuationAndWhitespace(t *testing.T) {
	sp := readSearchFixture(t)

	tests := []struct {
		query string
		want  []wantHit
	}{
		{
			query: "hello, WORLD",
			want: []wantHit{
				{subIndex: 3, timeIn: 10 * time.Second, matched: "Hello, world"},
				{subIndex: 4, timeIn: 13 * time.Second, matched: "Hello, world"},
			},
		},
		{
			// Punctuation of the text is ignored too.
			query: "hello world",
			want: []wantHit{
				{subIndex: 3, timeIn: 10 * time.Second, matched: "Hello, world"},
				{subIndex: 4, timeIn: 13 * time.Second, matched: "Hello, world"},
			},
		},
		{
			// Apostrophe ignored, case folded.
			query: "WERE MAKING",
			want: []wantHit{
				{subIndex: 0, timeIn: time.Second, matched: "we're making"},
			},
		},
	}

	for _, tc := range tests {
		res, err := sp.Search(tc.query)
		if err != nil {
			t.Fatalf("Search(%q): %v", tc.query, err)
		}
		checkHits(t, res, tc.want)
	}
}

func TestSearchAcrossLines(t *testing.T) {
	sp := readSearchFixture(t)

	// "making our final" spans the line break in subtitle 1.
	res, err := sp.Search("making our final")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 0, timeIn: time.Second, matched: "making\nour final"},
	})
	if h := res.Hits[0]; h.Start.Line != 0 || h.End.Line != 1 {
		t.Errorf("cross-line hit lines = %d..%d, want 0..1", h.Start.Line, h.End.Line)
	}

	// "always" alone is on the second line.
	res, err = sp.Search("always")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 2, timeIn: 7 * time.Second, matched: "Always"},
	})
	if h := res.Hits[0]; h.Start.Line != 1 || h.Start.RuneOffset != 0 {
		t.Errorf("Start = %+v, want line 1 rune 0", h.Start)
	}

	// A multi-word query must not match inside a single word even though
	// whitespace/punctuation normalization is in effect. "the a" normalizes
	// to "thea", which occurs inside "theater", but the query boundary after
	// "the" is not a word boundary there.
	sp2 := &SubsPack{Subs: []*Subtitle{
		{TimeIn: 0, TimeOut: time.Second, Lines: []string{"theater"}},
	}}
	if res, err = sp2.Search("the a"); err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("multi-word query matched inside a word: %q", res.Hits[0].MatchedText())
	}
	// The same text with a real word boundary does match.
	sp2.Subs[0].Lines = []string{"the a"}
	sp2.InvalidateSearchIndex()
	if res, err = sp2.Search("the a"); err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Errorf("query did not match at a real boundary")
	}
}

func TestSearchMixedCJK(t *testing.T) {
	sp := readSearchFixture(t)

	res, err := sp.Search("你好世界")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 5, timeIn: 16 * time.Second, matched: "你好，世界"},
	})

	// With the ideographic punctuation present in the query as well.
	res, err = sp.Search("你好，世界！")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 5, timeIn: 16 * time.Second, matched: "你好，世界"},
	})

	// The phrase on the second line.
	res, err = sp.Search("欢迎回来")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 5, timeIn: 16 * time.Second, matched: "欢迎回来"},
	})
	if h := res.Hits[0]; h.Start.Line != 1 || h.Start.RuneOffset != 0 || h.End.RuneOffset != 4 {
		t.Errorf("CJK position = %+v..%+v, want line 1 runes 0..4", h.Start, h.End)
	}
}

func TestSearchDuplicateDialogue(t *testing.T) {
	sp := readSearchFixture(t)

	// Both duplicates are reported, in subtitle order, with distinct times.
	res, err := sp.Search("may the force be with you")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 6, timeIn: 19 * time.Second, matched: "May the Force\nbe with you"},
		{subIndex: 8, timeIn: 25 * time.Second, matched: "May the Force be with you"},
	})
}

func TestSearchIgnoresMarkup(t *testing.T) {
	sp := readSearchFixture(t)

	// Subtitle 7 is "<i>May the Force</i>\nbe with you."; the HTML must not
	// be part of the visible text or of the reported positions.
	res, err := sp.Search("force be")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 6, timeIn: 19 * time.Second, matched: "Force\nbe"},
		{subIndex: 8, timeIn: 25 * time.Second, matched: "Force be"},
	})
	if h := res.Hits[0]; h.Start.Line != 0 || h.End.Line != 1 || h.End.RuneOffset != 2 {
		t.Errorf("HTML hit position = %+v..%+v", h.Start, h.End)
	}
	// The visible text must not include the tags.
	if got := VisibleText("<i>May the Force</i>"); got != "May the Force" {
		t.Errorf("VisibleText = %q, want %q", got, "May the Force")
	}
}

func TestSearchOverlapAndPositions(t *testing.T) {
	sp := &SubsPack{Subs: []*Subtitle{
		{TimeIn: 0, TimeOut: time.Second, Lines: []string{"ababa"}},
	}}
	res, err := sp.Search("aba")
	if err != nil {
		t.Fatal(err)
	}
	checkHits(t, res, []wantHit{
		{subIndex: 0, timeIn: 0, matched: "aba"},
		{subIndex: 0, timeIn: 0, matched: "aba"},
	})
	if res.Hits[0].Start.RuneOffset != 0 || res.Hits[1].Start.RuneOffset != 2 {
		t.Errorf("overlap positions = %d, %d; want 0, 2",
			res.Hits[0].Start.RuneOffset, res.Hits[1].Start.RuneOffset)
	}

	// Positions are rune offsets in the visible line, not byte offsets.
	sp = &SubsPack{Subs: []*Subtitle{
		{TimeIn: 0, TimeOut: time.Second, Lines: []string{"你好 世界"}},
	}}
	res, err = sp.Search("世界")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	if got := res.Hits[0].Start.RuneOffset; got != 3 { // 你(0) 好(1) space(2) 世(3)
		t.Errorf("rune offset = %d, want 3", got)
	}
}

func TestSearchEmptyAndInvalidQuery(t *testing.T) {
	sp := readSearchFixture(t)

	for _, q := range []string{"", "   ", "!!!...", "<i></i>", "{\\an8}"} {
		if _, err := sp.Search(q); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("Search(%q) error = %v, want ErrEmptyQuery", q, err)
		}
	}

	// A query whose normalized form exceeds the limit is rejected.
	if _, err := sp.SearchLimit(strings.Repeat("a", DefaultMaxQueryRunes+1), 0); !errors.Is(err, ErrQueryTooLong) {
		t.Errorf("long query error = %v, want ErrQueryTooLong", err)
	}
}

func TestSearchInvalidTime(t *testing.T) {
	sp := &SubsPack{Subs: []*Subtitle{
		{TimeIn: time.Second, TimeOut: 0, Lines: []string{"broken time"}},
	}}
	res, err := sp.Search("broken")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || !res.Hits[0].InvalidTime {
		t.Fatalf("hit = %+v, want one hit with InvalidTime", res.Hits)
	}
	// Times are still reported as stored, without exaggeration.
	if res.Hits[0].TimeIn != time.Second || res.Hits[0].TimeOut != 0 {
		t.Errorf("times = %v..%v, want 1s..0", res.Hits[0].TimeIn, res.Hits[0].TimeOut)
	}
}

func TestSearchBadTimestampsAreSkippedNotFatal(t *testing.T) {
	// A malformed timestamp line leaves a zero-time subtitle; search must not
	// panic and must not silently invent a time.
	src := "1\nnot a timestamp\nhello there\n\n"
	sp, err := ReadSrtFrom(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	res, err := sp.Search("hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || !res.Hits[0].InvalidTime {
		t.Fatalf("hits = %+v, want one invalid-time hit", res.Hits)
	}
}

func TestSearchIndexSizeLimit(t *testing.T) {
	sp := &SubsPack{Subs: []*Subtitle{
		{TimeIn: 0, TimeOut: time.Second, Lines: []string{strings.Repeat("x", 100)}},
	}}
	if _, err := BuildSearchIndexWithOptions(sp, SearchOptions{MaxIndexRunes: 10}); !errors.Is(err, ErrIndexTooLarge) {
		t.Errorf("error = %v, want ErrIndexTooLarge", err)
	}
	// The default limit accommodates small files.
	if _, err := BuildSearchIndex(sp); err != nil {
		t.Errorf("default build failed: %v", err)
	}
}

func TestSearchHitLimit(t *testing.T) {
	var subs []*Subtitle
	for i := 0; i < 20; i++ {
		subs = append(subs, &Subtitle{
			TimeIn:  time.Duration(i) * time.Second,
			TimeOut: time.Duration(i+1) * time.Second,
			Lines:   []string{"repeat me"},
		})
	}
	sp := &SubsPack{Subs: subs}

	res, err := sp.SearchLimit("repeat", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 5 || !res.Truncated {
		t.Errorf("len = %d, truncated = %v; want 5, true", len(res.Hits), res.Truncated)
	}

	// Exactly at the limit is not truncated.
	res, err = sp.SearchLimit("repeat", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 20 || res.Truncated {
		t.Errorf("len = %d, truncated = %v; want 20, false", len(res.Hits), res.Truncated)
	}
}

func TestSearchIndexReuseAndInvalidation(t *testing.T) {
	sp := readSearchFixture(t)

	idx1, err := sp.SearchIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := sp.SearchIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx1 != idx2 {
		t.Fatal("SearchIndex did not reuse the cached index")
	}

	// Time-only transformations do not invalidate the index, and the index
	// reports live timestamps.
	if _, err := idx1.Search("hello"); err != nil {
		t.Fatal(err)
	}
	sp.Shift(time.Minute)
	res, err := idx1.Search("hello")
	if err != nil {
		t.Fatalf("index invalidated by a time shift: %v", err)
	}
	if res.Hits[0].TimeIn != time.Minute+10*time.Second {
		t.Errorf("TimeIn = %v, want live 1m10s", res.Hits[0].TimeIn)
	}

	// A standalone index detects a direct text edit and reports ErrStaleIndex.
	sp.Subs[0].Lines[0] = "changed"
	if _, err := idx1.Search("hello"); !errors.Is(err, ErrStaleIndex) {
		t.Errorf("error = %v, want ErrStaleIndex", err)
	}

	// The cached index is rebuilt automatically.
	res, err = sp.Search("changed")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits after edit, want 1", len(res.Hits))
	}

	// Removal is detected by the length change and the cache is rebuilt.
	sp.RemoveHTML() // invalidates explicitly
	if sp.searchIndex != nil {
		t.Error("RemoveHTML did not invalidate the cached index")
	}
	if _, err := sp.Search("hello"); err != nil {
		t.Errorf("search after RemoveHTML: %v", err)
	}
}

func TestSearchDoesNotModifySubtitles(t *testing.T) {
	sp := readSearchFixture(t)

	var before bytes.Buffer
	if err := WriteSrtTo(&before, sp); err != nil {
		t.Fatal(err)
	}

	if _, err := sp.Search("force"); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Search("你好"); err != nil {
		t.Fatal(err)
	}

	var after bytes.Buffer
	if err := WriteSrtTo(&after, sp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Error("search modified the subtitles; the serialized output changed")
	}
}

func TestSearchParseUnchanged(t *testing.T) {
	// The fixture reparsed and re-serialized must keep all 9 subtitles, the
	// cross-line texts and the times, i.e. the existing parse behaviour is
	// untouched by the search feature.
	sp := readSearchFixture(t)
	if len(sp.Subs) != 9 {
		t.Fatalf("parsed %d subtitles, want 9", len(sp.Subs))
	}
	first := sp.Subs[0]
	if len(first.Lines) != 2 || first.Lines[0] != "Senator, we're making" || first.Lines[1] != "our final approach into Coruscant." {
		t.Errorf("first subtitle lines = %q", first.Lines)
	}
	if first.TimeIn != time.Second || first.TimeOut != 3500*time.Millisecond {
		t.Errorf("first subtitle times = %v..%v", first.TimeIn, first.TimeOut)
	}

	var buf bytes.Buffer
	if err := WriteSrtTo(&buf, sp); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Senator, we're making",
		"our final approach into Coruscant.",
		"Hello, world!",
		"你好，世界！",
		"<i>May the Force</i>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("serialized output is missing %q", want)
		}
	}

	// Re-reading the serialized output must find the same hits.
	sp2, err := ReadSrtFrom(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	res1, err := sp.Search("hello world")
	if err != nil {
		t.Fatal(err)
	}
	res2, err := sp2.Search("hello world")
	if err != nil {
		t.Fatal(err)
	}
	if len(res1.Hits) != len(res2.Hits) {
		t.Errorf("round trip changed hit count: %d vs %d", len(res1.Hits), len(res2.Hits))
	}
}

func TestSearchVeryLongFile(t *testing.T) {
	// A long file must be searchable and the index must be bounded.
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString(strings.Repeat("line ", 10))
		b.WriteByte('\n')
	}
	sp, err := ReadSrtFrom(strings.NewReader("1\n00:00:00,000 --> 00:00:01,000\n" + b.String() + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := sp.Search("line line line")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits in a long file")
	}
	if len(res.Hits) > DefaultMaxHits {
		t.Errorf("returned %d hits, above the default limit", len(res.Hits))
	}
}

func TestSearchFileNotFoundStillErrors(t *testing.T) {
	if _, err := ReadSrtFile("testdata/does-not-exist.srt"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if _, err := os.Stat("testdata/search.srt"); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
}

func TestSearchEdgeCases(t *testing.T) {
	// A nil pack does not panic.
	var nilPack *SubsPack
	if _, err := nilPack.Search("x"); !errors.Is(err, ErrNilSubsPack) {
		t.Errorf("nil pack error = %v, want ErrNilSubsPack", err)
	}
	nilPack.InvalidateSearchIndex() // Must not panic.

	// Empty pack.
	empty := &SubsPack{}
	res, err := empty.Search("anything")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("empty pack returned %d hits", len(res.Hits))
	}

	// Subtitle with no lines is indexed but never matches.
	sp := &SubsPack{Subs: []*Subtitle{{TimeIn: 0, TimeOut: time.Second}}}
	res, err = sp.Search("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("no-line subtitle returned hits")
	}

	// A query with spaces in a CJK phrase, where the text uses punctuation
	// at the same place, matches because punctuation is a word boundary.
	sp = &SubsPack{Subs: []*Subtitle{
		{TimeIn: 0, TimeOut: time.Second, Lines: []string{"你好，世界"}},
	}}
	res, err = sp.Search("你好 世界")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].MatchedText() != "你好，世界" {
		t.Errorf("CJK spaced query hits = %+v", res.Hits)
	}

	// A word-boundary violation on the second word is rejected.
	sp = &SubsPack{Subs: []*Subtitle{
		{TimeIn: 0, TimeOut: time.Second, Lines: []string{"helloworld"}},
	}}
	res, err = sp.Search("hello world")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("query matched across a missing word boundary: %+v", res.Hits)
	}

	// The single-word form does match.
	res, err = sp.Search("helloworld")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Errorf("single-word query did not match")
	}
}

func TestSearchOffsetsMidLineAndAcrossCJK(t *testing.T) {
	sp := &SubsPack{Subs: []*Subtitle{
		{TimeIn: 0, TimeOut: time.Second, Lines: []string{"prefix 你好，世界 suffix"}},
	}}
	res, err := sp.Search("世界 suffix")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits", len(res.Hits))
	}
	h := res.Hits[0]
	// Visible line: p(0) r(1) e(2) f(3) i(4) x(5) space(6) 你(7) 好(8) ，(9) 世(10) 界(11) space(12) suffix
	if h.Start.RuneOffset != 10 || h.End.RuneOffset != 19 {
		t.Errorf("offsets = %d..%d, want 10..19", h.Start.RuneOffset, h.End.RuneOffset)
	}
	if got := h.MatchedText(); got != "世界 suffix" {
		t.Errorf("MatchedText = %q", got)
	}
}

func TestSearchAfterStatsRebuilds(t *testing.T) {
	// Stats mutates the subtitles directly (through Subtitle methods), so the
	// cached index is stale afterwards. The fingerprint check must detect it
	// and rebuild, and the stripped text must be searchable.
	sp := readSearchFixture(t)
	if _, err := sp.Search("i>may"); err != nil { // Warm the cache.
		t.Fatal(err)
	}
	sp.Stats()

	res, err := sp.Search("may the force")
	if err != nil {
		t.Fatalf("search after stats: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("got %d hits after stats, want 2", len(res.Hits))
	}
	// The HTML was stripped by Stats, so the visible text is unchanged but
	// the raw lines no longer contain the tags; MatchedText is still right.
	if got := res.Hits[0].MatchedText(); got != "May the Force" {
		t.Errorf("MatchedText after stats = %q", got)
	}
	// A cross-line match is still reported across both lines.
	res, err = sp.Search("may the force be with you")
	if err != nil {
		t.Fatalf("search after stats: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("got %d cross-line hits after stats, want 2", len(res.Hits))
	}
	if got := res.Hits[0].MatchedText(); got != "May the Force\nbe with you" {
		t.Errorf("cross-line MatchedText after stats = %q", got)
	}
}

func TestSearchSymmetricalAcrossLineAndSpace(t *testing.T) {
	// "one two" written as "one\ntwo" matches, and the hit reports both lines.
	sp := &SubsPack{Subs: []*Subtitle{
		{TimeIn: 2 * time.Second, TimeOut: 3 * time.Second, Lines: []string{"one", "two"}},
	}}
	res, err := sp.Search("one two")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits", len(res.Hits))
	}
	h := res.Hits[0]
	if h.Start != (TextPosition{Line: 0, RuneOffset: 0}) ||
		h.End != (TextPosition{Line: 1, RuneOffset: 3}) {
		t.Errorf("positions = %+v..%+v", h.Start, h.End)
	}
}
