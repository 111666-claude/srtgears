/*
This file implements a reusable full-text index over the visible text of a
SubsPack, and a phrase / multi-word search that returns the matching subtitle
entries, their time intervals and the positions of the matches in the text.

Searching is normalization based:

  - case is folded using Unicode simple lower casing,
  - Unicode punctuation is ignored (both ASCII and CJK punctuation),
  - all Unicode whitespace is ignored (including the ideographic space U+3000),
  - HTML tags (e.g. <i>, <font color="red">) and inline control tags
    (e.g. {\an8}) are not visible text, so they are ignored as well.

Ignoring whitespace also means that a phrase is matched across subtitle line
breaks, and that an English multi-word query matches regardless of how the
words are split into lines. Punctuation is ignored the same way, so
"Hello, world!" matches the query "hello world".

A query with words separated by whitespace is matched as a word sequence: each
boundary between query words (and the start of the phrase) must fall on a word
boundary of the subtitle text (a line break, whitespace or punctuation). This
prevents a multi-word query such as "he at" from matching inside the single
word "theater", while still allowing the query to span subtitle lines.

CJK text has no spaces, so every CJK character is treated as a word start. As a
result a CJK phrase can be searched with or without spaces and punctuation:
"你好世界", "你好 世界" and "你好，世界" all match. A space-free query imposes
no internal boundaries at all for any script, so it also works as a plain
substring search.

The index is immutable with respect to the visible text: building an index does
not modify the SubsPack in any way. Timestamps are always read from the live
Subtitles, so time-only transformations (Shift, Scale, Lengthen) do not change
the returned time intervals. Changes to text or to the set of subtitles
invalidate an index; a standalone index then returns ErrStaleIndex. A SubsPack
also caches its default index, and that cache is dropped automatically by all
SubsPack-level transformations. If subtitle lines are edited directly (through
Subtitle.Lines, or through methods of Subtitle), call
SubsPack.InvalidateSearchIndex() so the cached index is dropped (a direct edit
is also detected and rebuilt automatically by SearchIndex).

Resource bounds: the amount of normalized text indexed is capped
(DefaultMaxIndexRunes), as are the query length and the number of returned
hits. Exceeding them is reported explicitly (ErrIndexTooLarge,
ErrQueryTooLong) or by setting SearchResults.Truncated instead of silently
exhausting memory or time.
*/
package srtgears

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Default resource limits used by search.
const (
	// DefaultMaxIndexRunes is the default maximum number of normalized runes
	// stored in a SearchIndex.
	DefaultMaxIndexRunes = 1 << 20
	// DefaultMaxQueryRunes is the default maximum length of a normalized query.
	DefaultMaxQueryRunes = 512
	// DefaultMaxHits is the default maximum number of hits returned by a search.
	DefaultMaxHits = 1000
)

// Search related errors.
var (
	// ErrEmptyQuery is returned when a query contains no searchable characters
	// (only whitespace, punctuation, markup or nothing at all).
	ErrEmptyQuery = errors.New("srtgears: empty search query")
	// ErrQueryTooLong is returned when the normalized query exceeds the
	// configured query length limit.
	ErrQueryTooLong = errors.New("srtgears: search query too long")
	// ErrIndexTooLarge is returned when the normalized text of the subtitle
	// pack exceeds the configured index size limit.
	ErrIndexTooLarge = errors.New("srtgears: subtitle text too large to index")
	// ErrStaleIndex is returned when an index is used after its subtitle pack
	// was modified. Rebuild the index, or use the cached SubsPack index.
	ErrStaleIndex = errors.New("srtgears: search index is stale")
	// ErrNilSubsPack is returned when a method requiring a subtitle pack is
	// called on nil.
	ErrNilSubsPack = errors.New("srtgears: nil subtitle pack")
)

// SearchOptions controls the resource limits of a SearchIndex.
// Zero values select the corresponding Default* constant.
type SearchOptions struct {
	MaxIndexRunes int // Maximum number of normalized runes to index.
	MaxQueryRunes int // Maximum length of a normalized query, in runes.
	MaxHits       int // Maximum number of hits returned by Search.
}

func (o SearchOptions) withDefaults() SearchOptions {
	if o.MaxIndexRunes <= 0 {
		o.MaxIndexRunes = DefaultMaxIndexRunes
	}
	if o.MaxQueryRunes <= 0 {
		o.MaxQueryRunes = DefaultMaxQueryRunes
	}
	if o.MaxHits <= 0 {
		o.MaxHits = DefaultMaxHits
	}
	return o
}

// TextPosition identifies a position in the visible text of a subtitle.
//
// Line is the zero-based index into Subtitle.Lines. RuneOffset is the
// zero-based offset in runes in the visible line (HTML tags and control tags
// removed). An End position is exclusive, i.e. it points right after the last
// matched rune.
type TextPosition struct {
	Line       int
	RuneOffset int
}

// SearchHit is a single match of a search query in a subtitle.
type SearchHit struct {
	SubIndex    int           // Index of the subtitle in SubsPack.Subs.
	Subtitle    *Subtitle     // The matching subtitle (not copied).
	TimeIn      time.Duration // Subtitle appearance time.
	TimeOut     time.Duration // Subtitle disappearance time.
	Start       TextPosition  // Start of the match (inclusive).
	End         TextPosition  // End of the match (exclusive).
	InvalidTime bool          // True if the subtitle has no valid time interval.
}

// SearchResults is the result of a search.
type SearchResults struct {
	Query           string      // The original query.
	NormalizedQuery string      // The normalized query that was matched.
	Hits            []SearchHit // Hits in subtitle order, then position order.
	Truncated       bool        // True if more hits existed beyond the hit limit.
}

// indexedSub is the per-subtitle part of a SearchIndex.
type indexedSub struct {
	subIndex int // Index in SubsPack.Subs.
	sub      *Subtitle
	norm     string  // Normalized visible text.
	line     []int32 // Line index for each normalized rune.
	off      []int32 // Visible rune offset for each normalized rune.
	bound    []bool  // Whether each normalized rune starts a new word.
}

// SearchIndex is a reusable index over the visible text of a SubsPack.
// It is not safe for concurrent use, and it must be rebuilt after the
// subtitle pack is modified (see InvalidateSearchIndex).
type SearchIndex struct {
	sp            *SubsPack
	rev           uint64 // SubsPack revision the index was built from.
	subsLen       int    // Number of subtitles at build time.
	fingerprint   uint64 // Fingerprint of the subtitle text at build time.
	entries       []indexedSub
	maxQueryRunes int
	maxHits       int
}

// textFingerprint computes a fingerprint of the subtitle text (not timestamps)
// that is used to detect modifications performed without invalidating the
// cached index. It is a 64-bit FNV-1a hash over the lines, each prefixed by
// its length so that line boundaries matter.
func textFingerprint(sp *SubsPack) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	mix := func(v uint64) {
		for i := 0; i < 8; i++ {
			h ^= v & 0xff
			h *= prime64
			v >>= 8
		}
	}
	if sp == nil {
		return h
	}
	mix(uint64(len(sp.Subs)))
	for _, sub := range sp.Subs {
		if sub == nil {
			mix(0)
			continue
		}
		mix(uint64(len(sub.Lines)))
		for _, line := range sub.Lines {
			mix(uint64(len(line)))
			for i := 0; i < len(line); i++ {
				h ^= uint64(line[i])
				h *= prime64
			}
		}
	}
	return h
}

// BuildSearchIndex builds a reusable search index for sp using the default
// resource limits.
func BuildSearchIndex(sp *SubsPack) (*SearchIndex, error) {
	return BuildSearchIndexWithOptions(sp, SearchOptions{})
}

// BuildSearchIndexWithOptions builds a reusable search index for sp using the
// supplied resource limits.
func BuildSearchIndexWithOptions(sp *SubsPack, opts SearchOptions) (*SearchIndex, error) {
	opts = opts.withDefaults()

	idx := &SearchIndex{
		sp:            sp,
		maxQueryRunes: opts.MaxQueryRunes,
		maxHits:       opts.MaxHits,
	}
	if sp == nil {
		return idx, nil
	}

	idx.rev = sp.searchRev
	idx.subsLen = len(sp.Subs)
	idx.fingerprint = textFingerprint(sp)

	totalRunes := 0
	for i, sub := range sp.Subs {
		if sub == nil {
			continue
		}

		e := indexedSub{subIndex: i, sub: sub}
		var normal []rune
		for lineIdx, line := range sub.Lines {
			visible := visibleText(line)
			offset := 0
			separated := true // The beginning of a line is a word boundary.
			for _, r := range visible {
				if unicode.IsSpace(r) || unicode.IsPunct(r) {
					separated = true
				} else {
					totalRunes++
					if totalRunes > opts.MaxIndexRunes {
						return nil, fmt.Errorf("%w: more than %d normalized runes",
							ErrIndexTooLarge, opts.MaxIndexRunes)
					}
					normal = append(normal, unicode.ToLower(r))
					e.line = append(e.line, int32(lineIdx))
					e.off = append(e.off, int32(offset))
					// Every CJK character is a word start, since those
					// scripts are written without spaces.
					e.bound = append(e.bound, separated || isCJK(r))
					separated = false
				}
				offset++
			}
		}
		if len(normal) > 0 {
			e.norm = string(normal)
		}
		idx.entries = append(idx.entries, e)
	}

	return idx, nil
}

// visibleText returns the visible text of a subtitle line: HTML tags and
// control tags are removed.
func visibleText(line string) string {
	line = htmlPattern.ReplaceAllString(line, "")
	return controlPattern.ReplaceAllString(line, "")
}

// VisibleText returns the visible text of a subtitle line: HTML tags (such as
// <i> or <font>) and inline control tags (such as {\an8}) are removed.
// This is exactly the text that search operates on.
func VisibleText(line string) string {
	return visibleText(line)
}

// MatchedText returns the visible text covered by the hit. Line breaks are
// represented by newline characters.
func (h *SearchHit) MatchedText() string {
	if h.Subtitle == nil || h.Start.Line < 0 || h.End.Line >= len(h.Subtitle.Lines) ||
		h.End.Line < h.Start.Line {
		return ""
	}
	var b strings.Builder
	for line := h.Start.Line; line <= h.End.Line; line++ {
		visible := []rune(visibleText(h.Subtitle.Lines[line]))
		from, to := 0, len(visible)
		if line == h.Start.Line && h.Start.RuneOffset > from {
			from = h.Start.RuneOffset
		}
		if line == h.End.Line && h.End.RuneOffset < to {
			to = h.End.RuneOffset
		}
		if from > to || from > len(visible) || to > len(visible) {
			return ""
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(string(visible[from:to]))
	}
	return b.String()
}

// isCJK reports whether r belongs to a script that is written without spaces
// between words, so that every such character acts as a word boundary.
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana,
		unicode.Hangul, unicode.Bopomofo)
}

// normalizeQuery normalizes a query the same way subtitle text is normalized.
// It returns the rune indexes in the normalized query that must fall on a word
// boundary of the text. It stops with ErrQueryTooLong as soon as the limit is
// exceeded. A space-free query returns no boundaries.
func normalizeQuery(query string, maxRunes int) (norm string, boundaries []int, err error) {
	visible := visibleText(query)
	var b strings.Builder
	count := 0
	separated := true // Leading whitespace does not start a word.
	for _, r := range visible {
		if unicode.IsSpace(r) {
			separated = true
			continue
		}
		if unicode.IsPunct(r) {
			// Punctuation is ignored but does not split a word:
			// "we're" and "were" are normalized the same way.
			continue
		}
		if count >= maxRunes {
			return "", nil, fmt.Errorf("%w: more than %d normalized runes",
				ErrQueryTooLong, maxRunes)
		}
		if separated && count > 0 {
			boundaries = append(boundaries, count)
		}
		b.WriteRune(unicode.ToLower(r))
		count++
		separated = false
	}
	return b.String(), boundaries, nil
}

// SearchIndex returns the cached search index of sp, building it if needed.
// The cache is dropped automatically by SubsPack transformations, and a
// direct edit of the subtitle text is detected and triggers a rebuild.
func (sp *SubsPack) SearchIndex() (*SearchIndex, error) {
	if sp == nil {
		return nil, ErrNilSubsPack
	}
	if idx := sp.searchIndex; idx != nil && idx.rev == sp.searchRev &&
		idx.subsLen == len(sp.Subs) &&
		idx.fingerprint == textFingerprint(sp) {
		return idx, nil
	}
	idx, err := BuildSearchIndex(sp)
	if err != nil {
		return nil, err
	}
	sp.searchIndex = idx
	return idx, nil
}

// InvalidateSearchIndex drops the cached search index of sp.
//
// SubsPack transformations invalidate the cache automatically. This method is
// needed after editing subtitle lines directly (for example assigning to
// Subtitle.Lines or calling a method of Subtitle).
func (sp *SubsPack) InvalidateSearchIndex() {
	if sp == nil {
		return
	}
	sp.searchRev++
	sp.searchIndex = nil
}

// Search searches sp using its cached index (building it if needed).
func (sp *SubsPack) Search(query string) (*SearchResults, error) {
	return sp.SearchLimit(query, 0)
}

// SearchLimit is like Search but limits the number of hits to maxHits.
// A non-positive maxHits selects the index's configured limit.
func (sp *SubsPack) SearchLimit(query string, maxHits int) (*SearchResults, error) {
	idx, err := sp.SearchIndex()
	if err != nil {
		return nil, err
	}
	return idx.SearchLimit(query, maxHits)
}

// Search searches the index using its configured hit limit.
func (idx *SearchIndex) Search(query string) (*SearchResults, error) {
	return idx.SearchLimit(query, 0)
}

// SearchLimit searches the index and limits the number of hits to maxHits.
// A non-positive maxHits selects the index's configured limit.
func (idx *SearchIndex) SearchLimit(query string, maxHits int) (*SearchResults, error) {
	if maxHits <= 0 {
		maxHits = idx.maxHits
	}

	normalized, boundaries, err := normalizeQuery(query, idx.maxQueryRunes)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return nil, ErrEmptyQuery
	}

	if idx.sp != nil &&
		(idx.sp.searchRev != idx.rev || len(idx.sp.Subs) != idx.subsLen ||
			textFingerprint(idx.sp) != idx.fingerprint) {
		return nil, ErrStaleIndex
	}

	queryRunes := utf8.RuneCountInString(normalized)
	res := &SearchResults{
		Query:           query,
		NormalizedQuery: normalized,
		Hits:            make([]SearchHit, 0, 8),
	}

	for i := range idx.entries {
		e := &idx.entries[i]
		if e.norm == "" {
			continue
		}

		// Convert byte offsets returned by strings.Index to rune offsets
		// incrementally: matches are found in increasing byte order.
		from := 0
		lastByte, lastRune := 0, 0
		for {
			rel := strings.Index(e.norm[from:], normalized)
			if rel < 0 {
				break
			}
			matchByte := from + rel
			lastRune += utf8.RuneCountInString(e.norm[lastByte:matchByte])
			lastByte = matchByte

			// Every boundary between query words must fall on a word
			// boundary of the text. A phrase that has boundaries must also
			// begin on a word boundary.
			wordOK := len(boundaries) == 0 || e.bound[lastRune]
			if wordOK {
				for _, qs := range boundaries {
					if !e.bound[lastRune+qs] {
						wordOK = false
						break
					}
				}
			}

			if wordOK {
				if len(res.Hits) >= maxHits {
					// Only report truncation when an actual extra match
					// was found.
					res.Truncated = true
					return res, nil
				}

				sub := e.sub
				res.Hits = append(res.Hits, SearchHit{
					SubIndex: e.subIndex,
					Subtitle: sub,
					TimeIn:   sub.TimeIn,
					TimeOut:  sub.TimeOut,
					Start: TextPosition{
						Line:       int(e.line[lastRune]),
						RuneOffset: int(e.off[lastRune]),
					},
					End: TextPosition{
						Line:       int(e.line[lastRune+queryRunes-1]),
						RuneOffset: int(e.off[lastRune+queryRunes-1]) + 1,
					},
					InvalidTime: sub.TimeIn < 0 || sub.TimeOut <= sub.TimeIn,
				})
			}

			// Advance by one rune so overlapping occurrences are also found.
			_, size := utf8.DecodeRuneInString(e.norm[matchByte:])
			from = matchByte + size
		}
	}

	return res, nil
}
