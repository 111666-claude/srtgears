/*
Package exec is a parameter driven executor which parses the parameters and performs subtitle transformations.

It is the heart of both the the srtgears command line tool and the web interface
(the web interface produces an argument list you would provide in the command line).
*/
package exec

import (
	"errors"
	"flag"
	"fmt"
	"github.com/icza/srtgears"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Executor is a helper type of which you can execute subtitle transformations defined by a series of arguments.
type Executor struct {
	FlagSet *flag.FlagSet // Custom Flagset used to parse parameters
	output  io.Writer     // Output used to write error messages and stats ('-stats' param)

	In         string  // input file name (*.srt)
	Out        string  // output file name (*.srt or *.ssa)
	In2        string  // optional 2nd input file name (when merging or concatenating subtitles) (*.srt)
	Out2       string  // optional 2nd output file name (when splitting) (*.srt or *.ssa)
	Concat     string  // concatenate 2 subtitle files, 2nd part start at e.g. '00:59:00,123'
	Merge      bool    // merge 2 subtitle files ('-in' at bottom, '-in2' at top
	SplitAt    string  // time at which to split to 2 subtitle files ('-out' and '-out2'), e.g. '00:59:00,123'
	ShiftBy    int     // shift subtitle timestamps (+/- ms)
	Scale      float64 // scale subtitle timestamps (faster/slower); multiplier e.g. 1.001
	Lengthen   float64 // lengthen / shorten display duration of subtitles, multiplier e.g. for +10% use 1.1
	RemoveHTML bool    // strip off formatting (e.g. <i>, <b>, <u>, <font> etc.)
	RemoveCtrl bool    // remove controls such as {\anX} (or {\aY}), {\pos(x,y)}
	RemoveHI   bool    // remove hearing impaired subtitles (such as '[PHONE RINGING]' or '(phone ringing)')
	Pos        string  // change subtitle position, one of: BL, B, BR, L, C, R, TL, T, TR  (B: bottom, T: Top, L: Left, R: Right, C: Center)
	Color      string  // change subtitle color, name (e.g. 'red' or 'yellow') or RGB hexa '#rrggbb' (e.g.'#ff0000' for red)
	Stats      bool    // analyze file and print statistics
	Search     string  // search phrase in the subtitles and print the matching time intervals
	SearchMax  int     // maximum number of search hits to print (0: default limit)

	Modified bool // Flag telling if transformation was performed on loaded subtitle(s) (set by GearIt())

	// Callback function to be called if stats "transformation" to be performed and no errors occurred.
	// Stats is special because it is the only transformation that produces output to Output (and not to file).
	BeforeStats func()

	// Callback function to be called if a search is to be performed and no errors occurred.
	// Search is special because it is the only "transformation" that produces output to Output
	// (and not to file), in the same way as Stats.
	BeforeSearch func()

	Sp1, Sp2 *srtgears.SubsPack // SubsPacks to operate on. Must be set by the user before calling GearIt()!
}

// New creates a new Executor.
func New(output io.Writer) *Executor {
	e := &Executor{
		FlagSet: flag.NewFlagSet("name", flag.ContinueOnError),
	}
	e.SetOutput(output)
	return e
}

// SetOutput sets the output for error messages and stats ('-stats' param).
func (e *Executor) SetOutput(output io.Writer) {
	e.output = output
	e.FlagSet.SetOutput(output)
}

// ProcFlags sets up variables for parsing the arguments, pointing to the fields of the Executor.
// And parses the arguments.
func (e *Executor) ProcFlags(arguments []string) error {
	f := e.FlagSet

	f.StringVar(&e.In, "in", "", "input file name (*.srt)")
	f.StringVar(&e.Out, "out", "", "output file name (*.srt or *.ssa)")
	f.StringVar(&e.In2, "in2", "", "optional 2nd input file name (when merging or concatenating subtitles) (*.srt)")
	f.StringVar(&e.Out2, "out2", "", "optional 2nd output file name (when splitting) (*.srt or *.ssa)")
	f.BoolVar(&srtgears.Debug, "debug", true, "print debug messages")
	f.StringVar(&e.Concat, "concat", "", "concatenate 2 subtitle files, 2nd part start at e.g. '00:59:00,123'")
	f.BoolVar(&e.Merge, "merge", false, "merge 2 subtitle files ('-in' at bottom, '-in2' at top)")
	f.StringVar(&e.SplitAt, "splitAt", "", "time at which to split to 2 subtitle files ('-out' and '-out2'), e.g. '00:59:00,123'")
	f.IntVar(&e.ShiftBy, "shiftBy", 0, "shift subtitle timestamps (+/- ms)")
	f.Float64Var(&e.Scale, "scale", 0, "scale subtitle timestamps (faster/slower); multiplier e.g. 1.001")
	f.Float64Var(&e.Lengthen, "lengthen", 0, "lengthen / shorten display duration of subtitles, multiplier e.g. for +10% use 1.1")
	f.BoolVar(&e.RemoveHTML, "removehtml", false, "strip off formatting (e.g. <i>, <b>, <u>, <font> etc.)")
	f.BoolVar(&e.RemoveCtrl, "removectrl", false, `remove controls such as {\anX} (or {\aY}), {\pos(x,y)}`)
	f.BoolVar(&e.RemoveHI, "removehi", false, "remove hearing impaired subtitles (such as '[PHONE RINGING]' or '(phone ringing)')")
	f.StringVar(&e.Pos, "pos", "", "change subtitle position, one of: BL, B, BR, L, C, R, TL, T, TR  (B: bottom, T: Top, L: Left, R: Right, C: Center)")
	f.StringVar(&e.Color, "color", "", "change subtitle color, name (e.g. 'red' or 'yellow') or RGB hexa '#rrggbb' (e.g.'#ff0000' for red)")
	f.BoolVar(&e.Stats, "stats", false, "analyze file and print statistics")
	f.StringVar(&e.Search, "search", "", "search a phrase in the subtitles and print the matching time intervals")
	f.IntVar(&e.SearchMax, "searchmax", 0, "maximum number of search hits to print, 0 means the default limit")

	return f.Parse(arguments)
}

// Regexp pattern used to parse timestamps.
var timestampPattern = regexp.MustCompile(`(\d\d):(\d\d):(\d\d)[,\.](\d\d\d)`)

// parseTime parses a timestamp which must be in the form of
// 00:00:00,000
func parseTime(t string) (time.Duration, error) {
	parts := timestampPattern.FindStringSubmatch(t)
	if len(parts) == 0 {
		return 0, fmt.Errorf("Invalid time: %s", t)
	}
	get := func(idx int) time.Duration {
		n, err := strconv.ParseInt(parts[idx], 10, 64)
		if err != nil {
			panic(err) // This shouldn't happen as only digits are matched.
		}
		return time.Duration(n)
	}
	return time.Hour*get(1) + time.Minute*get(2) + time.Second*get(3) + time.Millisecond*get(4), nil
}

// formatTime formats a timestamp the way SubRip does: 00:00:00,000.
func formatTime(t time.Duration) string {
	hour := t / time.Hour
	min := (t % time.Hour) / time.Minute
	sec := (t % time.Minute) / time.Second
	ms := (t % time.Second) / time.Millisecond
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hour, min, sec, ms)
}

// runSearch searches the visible text of sp1 and prints the hits to the output.
func (e *Executor) runSearch(sp1 *srtgears.SubsPack) error {
	if e.BeforeSearch != nil {
		e.BeforeSearch()
	}

	res, err := sp1.SearchLimit(e.Search, e.SearchMax)
	if err != nil {
		switch {
		case errors.Is(err, srtgears.ErrEmptyQuery):
			return fmt.Errorf("Invalid search phrase (no searchable characters): %q", e.Search)
		case errors.Is(err, srtgears.ErrQueryTooLong):
			return fmt.Errorf("Search phrase is too long: %q", e.Search)
		case errors.Is(err, srtgears.ErrIndexTooLarge):
			return fmt.Errorf("Subtitle text is too large to search: %v", err)
		default:
			return fmt.Errorf("Search failed: %v", err)
		}
	}

	fmt.Fprintf(e.output, "SEARCH of %s for %q:\n", e.In, res.Query)
	if len(res.Hits) == 0 {
		fmt.Fprintf(e.output, "No matches found.\n")
		return nil
	}

	for _, h := range res.Hits {
		timeRange := formatTime(h.TimeIn) + " --> " + formatTime(h.TimeOut)
		if h.InvalidTime {
			timeRange += " (invalid time)"
		}
		line := "?"
		if h.Subtitle != nil && h.Start.Line < len(h.Subtitle.Lines) {
			line = strconv.Itoa(h.Start.Line + 1)
		}
		fmt.Fprintf(e.output, "#%d %s (line %s, rune %d): %s\n",
			h.SubIndex+1, timeRange, line, h.Start.RuneOffset+1, visibleForOutput(h))
	}
	if res.Truncated {
		fmt.Fprintf(e.output, "Results truncated; only the first %d matches are shown.\n", len(res.Hits))
	}

	return nil
}

// visibleForOutput returns the matched text on a single line, suitable for a
// plain text report.
func visibleForOutput(h srtgears.SearchHit) string {
	return strings.ReplaceAll(h.MatchedText(), "\n", " / ")
}

// Mapping between positions expected in arguments to our model Pos.
var argPosToModelPos = map[string]srtgears.Pos{
	"TL": srtgears.TopLeft, "T": srtgears.Top, "TR": srtgears.TopRight,
	"L": srtgears.Left, "C": srtgears.Center, "R": srtgears.Right,
	"BL": srtgears.BottomLeft, "B": srtgears.Bottom, "BR": srtgears.BottomRight,
}

// GearIt performs subtitle transformations specified by the arguments passed to ProcFlags().
// Prior to calling this method, Executor.Sp1 and Executor.Sp2 should be set.
func (e *Executor) GearIt() (err error) {
	sp1, sp2 := e.Sp1, e.Sp2

	if sp1 == nil {
		return fmt.Errorf("Input file must be specified ('-in')!")
	}
	if sp2 == nil && (e.Concat != "" || e.Merge) {
		return fmt.Errorf("2nd input file must be specified ('-in2')!")
	}

	if e.Concat != "" {
		secPartStart, err := parseTime(e.Concat)
		if err != nil {
			return fmt.Errorf("Invalid time for concat: %s", e.Concat)
		}
		sp1.Concatenate(sp2, secPartStart)
		e.Modified = true
	}

	if e.Merge {
		sp1.Merge(sp2)
		e.Modified = true
	}

	if e.Lengthen != 0 {
		sp1.Lengthen(e.Lengthen)
		e.Modified = true
	}

	if e.RemoveCtrl {
		sp1.RemoveControl()
		e.Modified = true
	}

	if e.RemoveHI {
		sp1.RemoveHI()
		e.Modified = true
	}

	if e.RemoveHTML {
		sp1.RemoveHTML()
		e.Modified = true
	}

	if e.Pos != "" {
		pos2, ok := argPosToModelPos[e.Pos]
		if !ok {
			return fmt.Errorf("Invalid pos value: %s", e.Pos)
		}
		sp1.SetPos(pos2)
		e.Modified = true
	}

	if e.Color != "" {
		sp1.SetColor(e.Color)
		e.Modified = true
	}

	if e.Scale != 0 {
		sp1.Scale(e.Scale)
		e.Modified = true
	}

	if e.ShiftBy != 0 {
		sp1.Shift(time.Duration(e.ShiftBy) * time.Millisecond)
		e.Modified = true
	}

	if e.SplitAt != "" {
		at, err := parseTime(e.SplitAt)
		if err != nil {
			return fmt.Errorf("Invalid time for splitAt: %s", e.SplitAt)
		}
		sp2 = sp1.Split(at)
		e.Sp2 = sp2 // sp2 is just a local copy, so we need to update Executor.Sp2 too!
		e.Modified = true
	}

	// Search before stats: stats is destructive (it strips formatting and
	// hearing impaired lines), while search should report the visible text of
	// the transformed subtitles.
	if e.Search != "" {
		if err := e.runSearch(sp1); err != nil {
			return err
		}
	}

	if e.Stats {
		if e.BeforeStats != nil {
			e.BeforeStats()
		}
		ss := sp1.Stats()
		fmt.Fprintf(e.output, "STATS of %s:\n", e.In)
		p := func(name string, value interface{}) {
			fmt.Fprintf(e.output, "%-29s: %v\n", name, value)
		}
		p("Total # of subtitles", ss.Subs)
		p("Lines", ss.Lines)
		p("Avg lines per sub", fmt.Sprintf("%.4f", ss.AvgLinesPerSub))
		p("Chars (with spaces)", ss.Chars)
		p("Chars (without spaces)", ss.CharsNoSpace)
		p("Avg chars (no space) per line", fmt.Sprintf("%.4f", ss.AvgCharsPerLine))
		p("Words", ss.Words)
		p("Avg words per line", fmt.Sprintf("%.4f", ss.AvgWordsPerLine))
		p("Avg chars per word", fmt.Sprintf("%.4f", ss.AvgCharsPerWord))
		p("Total subtitle display time", ss.TotalDispDur)
		p("Subtitle visible ratio", fmt.Sprintf("%.2f%% (compared to total length)", ss.SubVisibRatio*100))
		p("Avg. display duration", ss.AvgDispDurPerNonSpaceChar.String()+" per 1 non-space char")
		p("Subs with HTML formatting", ss.HTMLs)
		p("Subs with controls", ss.Controls)
		p("Subs with hearing impaired", ss.HIs)
	}

	// Search and stats are analysis modes: they print their result to the
	// output instead of files, so no output file is required for them.
	if !e.Stats && e.Search == "" && e.Modified {
		// If there were modifications but no output file is specified, treat that as an error:
		if e.Out == "" {
			return fmt.Errorf("Output file must be specified ('-out')!")
		}
		if e.SplitAt != "" && e.Out2 == "" {
			return fmt.Errorf("Second output file must be specified ('-out2')!")
		}
	}

	return
}
