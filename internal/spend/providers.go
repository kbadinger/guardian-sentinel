package spend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Anthropic reads the Claude API cost report. It needs an Admin API key
// (sk-ant-admin01-...); workspace keys are refused by the endpoint.
// https://platform.claude.com/docs/en/manage-claude/usage-cost-api
type Anthropic struct {
	Label  string
	Key    string
	API    string // base URL; https://api.anthropic.com when empty
	Client *http.Client
}

func (a *Anthropic) Name() string { return a.Label }

func (a *Anthropic) Fetch(ctx context.Context, from, to time.Time) (Usage, error) {
	api := a.API
	if api == "" {
		api = "https://api.anthropic.com"
	}
	header := http.Header{}
	header.Set("x-api-key", a.Key)
	header.Set("anthropic-version", "2023-06-01")

	u := newUsage()
	page := ""
	for i := 0; i < 20; i++ { // a month is at most 31 daily buckets; 20 pages is a runaway guard
		q := url.Values{}
		q.Set("starting_at", from.UTC().Format(time.RFC3339))
		q.Set("ending_at", to.UTC().Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		q.Set("limit", "31")
		q.Add("group_by[]", "description")
		if page != "" {
			q.Set("page", page)
		}
		body, err := get(ctx, a.Client, api+"/v1/organizations/cost_report?"+q.Encode(), header)
		if err != nil {
			return Usage{}, err
		}
		var r struct {
			Data []struct {
				StartingAt string `json:"starting_at"`
				Results    []struct {
					Amount      string `json:"amount"` // USD cents, as a decimal string
					Model       string `json:"model"`
					Description string `json:"description"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool   `json:"has_more"`
			NextPage string `json:"next_page"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return Usage{}, fmt.Errorf("unexpected response: %w", err)
		}
		for _, b := range r.Data {
			day := dayOf(b.StartingAt)
			for _, res := range b.Results {
				cents, err := strconv.ParseFloat(res.Amount, 64)
				if err != nil {
					return Usage{}, fmt.Errorf("bad amount %q", res.Amount)
				}
				item := res.Model
				if item == "" {
					item = res.Description
				}
				u.add(day, item, cents/100)
			}
		}
		if !r.HasMore || r.NextPage == "" {
			return u, nil
		}
		page = r.NextPage
	}
	return Usage{}, fmt.Errorf("too many pages")
}

// Vercel reads a team's billing charges (FOCUS format, one JSON object per
// line). The token needs a role that may read billing.
// https://vercel.com/docs/rest-api/billing/list-focus-billing-charges
type Vercel struct {
	Label  string
	Key    string
	TeamID string
	API    string // base URL; https://api.vercel.com when empty
	Client *http.Client
}

func (v *Vercel) Name() string { return v.Label }

func (v *Vercel) Fetch(ctx context.Context, from, to time.Time) (Usage, error) {
	api := v.API
	if api == "" {
		api = "https://api.vercel.com"
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+v.Key)

	q := url.Values{}
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))
	q.Set("teamId", v.TeamID)
	body, err := get(ctx, v.Client, api+"/v1/billing/charges?"+q.Encode(), header)
	if err != nil {
		return Usage{}, err
	}
	u := newUsage()
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var c struct {
			ChargePeriodStart string  `json:"ChargePeriodStart"`
			BilledCost        float64 `json:"BilledCost"`
			BillingCurrency   string  `json:"BillingCurrency"`
			ServiceName       string  `json:"ServiceName"`
		}
		if err := json.Unmarshal(line, &c); err != nil {
			return Usage{}, fmt.Errorf("unexpected response: %w", err)
		}
		if c.BillingCurrency != "" && c.BillingCurrency != "USD" {
			return Usage{}, fmt.Errorf("unexpected currency %q", c.BillingCurrency)
		}
		u.add(dayOf(c.ChargePeriodStart), c.ServiceName, c.BilledCost)
	}
	if err := sc.Err(); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// dayOf turns an RFC 3339 timestamp into its UTC day.
func dayOf(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if len(ts) >= 10 {
			return ts[:10]
		}
		return ts
	}
	return t.UTC().Format(time.DateOnly)
}
