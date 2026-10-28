package spend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var (
	may1  = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	may29 = time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
)

func TestAnthropicConvertsCentsAndFollowsPages(t *testing.T) {
	var sawKey, sawGroup bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKey = r.Header.Get("x-api-key") == "admin-key" && r.Header.Get("anthropic-version") != ""
		sawGroup = r.URL.Query().Get("group_by[]") == "description"
		if r.URL.Path != "/v1/organizations/cost_report" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "" {
			fmt.Fprint(w, `{"data":[{"starting_at":"2026-05-27T00:00:00Z","results":[
				{"amount":"1250.5","model":"claude-opus-5-5"},{"amount":"50","model":"claude-haiku-4-5"}]}],
				"has_more":true,"next_page":"p2"}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"starting_at":"2026-05-28T00:00:00Z","results":[
			{"amount":"300","model":"","description":"Web Search Usage"}]}],"has_more":false}`)
	}))
	defer srv.Close()

	u, err := (&Anthropic{Label: "Anthropic", Key: "admin-key", API: srv.URL}).Fetch(context.Background(), may1, may29)
	if err != nil {
		t.Fatal(err)
	}
	if !sawKey || !sawGroup {
		t.Errorf("request headers or grouping wrong: key=%v group=%v", sawKey, sawGroup)
	}
	if got := u.ByDay["2026-05-27"]; math.Abs(got-13.005) > 1e-9 {
		t.Errorf("May 27 = %v, want 13.005 (cents → dollars)", got)
	}
	if got := u.ByItem["Web Search Usage"]; math.Abs(got-3) > 1e-9 {
		t.Errorf("description fallback = %v, want 3", got)
	}
	if top, _ := u.Top(); top != "claude-opus-5-5" {
		t.Errorf("top = %q", top)
	}
}

func TestVercelParsesJSONLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("teamId") != "team_1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprintln(w, `{"ChargePeriodStart":"2026-05-28T07:00:00.000Z","BilledCost":2.5,"BillingCurrency":"USD","ServiceName":"Build CPU Minutes"}`)
		fmt.Fprintln(w, ``)
		fmt.Fprintln(w, `{"ChargePeriodStart":"2026-05-27T07:00:00.000Z","BilledCost":0.25,"BillingCurrency":"USD","ServiceName":"Fluid Active CPU"}`)
	}))
	defer srv.Close()

	u, err := (&Vercel{Label: "Vercel", Key: "tok", TeamID: "team_1", API: srv.URL}).Fetch(context.Background(), may1, may29)
	if err != nil {
		t.Fatal(err)
	}
	if u.Total() != 2.75 || u.ByDay["2026-05-28"] != 2.5 {
		t.Errorf("total=%v byday=%v", u.Total(), u.ByDay)
	}
}

func TestHTTPErrorNeverLeaksTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := (&Vercel{Label: "Vercel", Key: "super-secret-token", TeamID: "t", API: srv.URL}).Fetch(context.Background(), may1, may29)
	if err == nil || err.Error() != "HTTP 401" {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Error("error contains the key")
	}
}

type fakeSource struct {
	name string
	u    Usage
	err  error
	from time.Time
	to   time.Time
}

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Fetch(_ context.Context, from, to time.Time) (Usage, error) {
	f.from, f.to = from, to
	return f.u, f.err
}

func TestCollectWindowAndFailingSource(t *testing.T) {
	ok := &fakeSource{name: "Anthropic", u: Usage{
		ByDay:  map[string]float64{"2026-05-27": 10, "2026-05-28": 1234.5},
		ByItem: map[string]float64{"claude-opus-5-5": 1200, "other": 44.5},
	}}
	bad := &fakeSource{name: "Vercel", err: errors.New("HTTP 403")}
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

	lines := Collect(context.Background(), []Source{bad, ok}, now)
	if !ok.from.Equal(may1) || !ok.to.Equal(may29) {
		t.Errorf("window = %s..%s, want May 1..May 29", ok.from, ok.to)
	}
	text := Format("fleet", now.AddDate(0, 0, -1), lines, []string{"OpenAI"})
	for _, want := range []string{
		"spend for Thu May 28",
		"• Anthropic: $1,234.50 yesterday · $1,244.50 this month (top: claude-opus-5-5 $1,200.00)",
		"• Vercel: couldn't read (HTTP 403)",
		"Total: $1,234.50 yesterday · $1,244.50 this month",
		"Not tracked yet: OpenAI",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "Anthropic") > strings.Index(text, "Vercel") {
		t.Error("biggest spender should be listed first")
	}
}

func TestMonthStartOnTheFirst(t *testing.T) {
	// On June 1 "yesterday" is May 31, so the month to date is May.
	f := &fakeSource{name: "x", u: Usage{ByDay: map[string]float64{}, ByItem: map[string]float64{}}}
	Collect(context.Background(), []Source{f}, time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC))
	if !f.from.Equal(may1) || !f.to.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window = %s..%s", f.from, f.to)
	}
}

func TestNextRunAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		now, want time.Time
	}{
		// before today's slot → today
		{time.Date(2026, 10, 28, 7, 0, 0, 0, ny), time.Date(2026, 10, 28, 8, 0, 0, 0, ny)},
		// exactly at the slot → tomorrow (never twice for one slot)
		{time.Date(2026, 10, 28, 8, 0, 0, 0, ny), time.Date(2026, 10, 29, 8, 0, 0, 0, ny)},
		// the night clocks fall back: still 08:00 local the next morning
		{time.Date(2026, 10, 31, 22, 0, 0, 0, ny), time.Date(2026, 11, 1, 8, 0, 0, 0, ny)},
	}
	for _, c := range cases {
		if got := NextRun(c.now, 8, 0, ny); !got.Equal(c.want) {
			t.Errorf("NextRun(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}

func TestCommas(t *testing.T) {
	for in, want := range map[float64]string{0: "$0.00", 12.3: "$12.30", 1234.5: "$1,234.50", 1234567.891: "$1,234,567.89"} {
		if got := usd(in); got != want {
			t.Errorf("usd(%v) = %s, want %s", in, got, want)
		}
	}
}
