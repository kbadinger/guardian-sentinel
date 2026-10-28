// Package spend reads what each billing provider charged and writes one
// plain-text daily report. It only reads: nothing here changes a limit,
// pauses a project or touches a key at a provider.
//
// Provider days are UTC days, because that is how the providers bucket
// their billing data.
package spend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Usage is one provider's spend over a window, in US dollars.
type Usage struct {
	ByDay  map[string]float64 // "2006-01-02" (UTC) → USD
	ByItem map[string]float64 // model or service name → USD
}

func newUsage() Usage {
	return Usage{ByDay: map[string]float64{}, ByItem: map[string]float64{}}
}

func (u Usage) add(day, item string, usd float64) {
	u.ByDay[day] += usd
	if item == "" {
		item = "other"
	}
	u.ByItem[item] += usd
}

// Total is the sum over the whole window.
func (u Usage) Total() float64 {
	var t float64
	for _, v := range u.ByDay {
		t += v
	}
	return t
}

// Top is the item that cost the most, or "" when nothing was spent.
func (u Usage) Top() (string, float64) {
	var name string
	var best float64
	for k, v := range u.ByItem {
		if v > best || (v == best && v > 0 && k < name) {
			name, best = k, v
		}
	}
	return name, best
}

// Source is one billing provider.
type Source interface {
	Name() string
	// Fetch returns spend from from (inclusive) to to (exclusive), both UTC
	// midnights.
	Fetch(ctx context.Context, from, to time.Time) (Usage, error)
}

// get does an authenticated GET and returns the body. Errors never include
// the request headers, which carry the key.
func get(ctx context.Context, client *http.Client, u string, header http.Header) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("could not build request")
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// Line is one provider's row in the report.
type Line struct {
	Name       string
	Yesterday  float64
	MonthToDay float64
	Top        string
	TopUSD     float64
	Err        error
}

// Collect reads every source for yesterday (UTC) and the month to date
// (UTC), as of now. A failing source becomes a Line with Err set; the
// others still report.
func Collect(ctx context.Context, sources []Source, now time.Time) []Line {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	monthStart := time.Date(yesterday.Year(), yesterday.Month(), 1, 0, 0, 0, 0, time.UTC)

	var lines []Line
	for _, s := range sources {
		u, err := s.Fetch(ctx, monthStart, today)
		l := Line{Name: s.Name(), Err: err}
		if err == nil {
			l.Yesterday = u.ByDay[yesterday.Format(time.DateOnly)]
			l.MonthToDay = u.Total()
			l.Top, l.TopUSD = u.Top()
		}
		lines = append(lines, l)
	}
	return lines
}

// Format writes the report. day is the UTC day the "yesterday" column
// covers; untracked names the providers not wired up yet.
func Format(instance string, day time.Time, lines []Line, untracked []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sentinel[%s] 💵 spend for %s (UTC day)\n", instance, day.Format("Mon Jan 2"))
	var yTotal, mTotal float64
	sorted := append([]Line(nil), lines...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].MonthToDay > sorted[j].MonthToDay })
	for _, l := range sorted {
		if l.Err != nil {
			fmt.Fprintf(&b, "• %s: couldn't read (%v)\n", l.Name, l.Err)
			continue
		}
		yTotal += l.Yesterday
		mTotal += l.MonthToDay
		fmt.Fprintf(&b, "• %s: %s yesterday · %s this month", l.Name, usd(l.Yesterday), usd(l.MonthToDay))
		if l.Top != "" && l.TopUSD >= 0.01 {
			fmt.Fprintf(&b, " (top: %s %s)", l.Top, usd(l.TopUSD))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Total: %s yesterday · %s this month", usd(yTotal), usd(mTotal))
	if len(untracked) > 0 {
		fmt.Fprintf(&b, "\nNot tracked yet: %s", strings.Join(untracked, ", "))
	}
	return b.String()
}

func usd(v float64) string {
	return "$" + commas(fmt.Sprintf("%.2f", v))
}

// commas puts thousands separators into a "1234.56" string.
func commas(s string) string {
	whole, frac, _ := strings.Cut(s, ".")
	neg := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	var out []byte
	for i := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, whole[i])
	}
	r := string(out) + "." + frac
	if neg {
		r = "-" + r
	}
	return r
}

// NextRun is the next time at hour:minute in loc strictly after now.
func NextRun(now time.Time, hour, minute int, loc *time.Location) time.Time {
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	if !next.After(n) {
		next = time.Date(n.Year(), n.Month(), n.Day()+1, hour, minute, 0, 0, loc)
	}
	return next
}
