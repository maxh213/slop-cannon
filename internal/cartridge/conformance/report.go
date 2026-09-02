package conformance

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Table renders the report as the pass/fail table the make target prints:
// a header naming the cartridge and what Detect saw, one row per check
// (name, result, time, detail or failure), and a totals line.
func (r Report) Table() string {
	var b strings.Builder
	r.WriteTable(&b)
	return b.String()
}

// WriteTable writes Table to w.
func (r Report) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "cartridge conformance: %s\n", r.Cartridge)
	if r.Status.Installed || r.Status.Version != "" || r.Status.Detail != "" {
		fmt.Fprintf(w, "detect: installed=%t version=%q auth=%s", r.Status.Installed, r.Status.Version, r.Status.Auth)
		if r.Status.Path != "" {
			fmt.Fprintf(w, " path=%s", r.Status.Path)
		}
		if r.Status.Detail != "" {
			fmt.Fprintf(w, " (%s)", r.Status.Detail)
		}
		fmt.Fprintln(w)
	}
	if r.Token != "" {
		fmt.Fprintf(w, "token: %s\n", r.Token)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tRESULT\tTIME\tDETAIL")
	for _, res := range r.Results {
		detail := res.Detail
		if res.Status == StatusFail && res.Err != nil {
			detail = "FAIL: " + oneLine(res.Err.Error())
			if res.Detail != "" {
				detail += " [" + res.Detail + "]"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", res.Check, strings.ToUpper(res.Status.String()), formatDuration(res.Duration), oneLine(detail))
	}
	_ = tw.Flush()
	pass, fail, skip := r.Counts()
	fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped in %s", pass, fail, skip, formatDuration(r.Duration))
	if u := r.Usage(); u.TotalTokens() > 0 || u.HasCost {
		fmt.Fprintf(w, "; %s", describeUsage(u))
	}
	fmt.Fprintln(w)
}

// formatDuration renders d with a resolution that suits its size.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(10 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

// oneLine folds newlines so a detail never breaks the table.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " | ")
	return strings.TrimSpace(s)
}
