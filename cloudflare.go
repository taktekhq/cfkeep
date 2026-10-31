package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// rowLimit is the most rows one Cloudflare GraphQL query returns.
const rowLimit = 10000

type client struct {
	api   string
	token string
	http  *http.Client
}

func newClient(token string) *client {
	return &client{api: "https://api.cloudflare.com/client/v4", token: token, http: &http.Client{Timeout: 60 * time.Second}}
}

type zone struct {
	ID, Name, AccountID string
}

var zoneName = regexp.MustCompile(`^[a-z0-9.-]+$`)

// zone looks a zone up by its domain name, for its zone tag and account tag.
func (c *client) zone(ctx context.Context, name string) (zone, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !zoneName.MatchString(name) {
		return zone{}, fmt.Errorf("%q is not a domain name", name)
	}
	var res struct {
		Result []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &res); err != nil {
		return zone{}, err
	}
	if len(res.Result) == 0 {
		return zone{}, errors.New("zone not found, or the token cannot read it (needs Zone:Read)")
	}
	r := res.Result[0]
	return zone{ID: r.ID, Name: r.Name, AccountID: r.Account.ID}, nil
}

func (c *client) graphql(ctx context.Context, query string) (json.RawMessage, error) {
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "/graphql", map[string]string{"query": query}, &res); err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		msgs := make([]string, len(res.Errors))
		for i, e := range res.Errors {
			msgs[i] = e.Message
		}
		return nil, errors.New(strings.Join(msgs, "; "))
	}
	return res.Data, nil
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	return withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, method, c.api+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return temporary{err}
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return temporary{err}
		}
		if resp.StatusCode/100 != 2 {
			err := fmt.Errorf("%s %s: HTTP %d: %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, truncate(string(b), 300))
			if retryable(resp.StatusCode) {
				return temporary{err}
			}
			return err
		}
		return json.Unmarshal(b, out)
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// pull fetches one dataset for one zone and one UTC day and writes it as one file.
// When the day reaches the row limit it is fetched again hour by hour and summed,
// so busy days are complete instead of cut at 10,000 rows.
func (p *puller) pull(ctx context.Context, z zone, d dataset, day time.Time) error {
	rows, err := p.fetch(ctx, z, d, day, day.AddDate(0, 0, 1))
	if err != nil {
		return err
	}
	if len(rows) >= p.limit && d.Hourly {
		var parts [][]row
		for h := 0; h < 24; h++ {
			from := day.Add(time.Duration(h) * time.Hour)
			part, err := p.fetch(ctx, z, d, from, from.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(part) >= p.limit {
				fmt.Fprintf(p.warn, "cfkeep: warning: %s %s %s hour %02d still has %d+ rows; the smallest groups are missing\n",
					z.Name, d.Name, day.Format(dateLayout), h, p.limit)
			}
			parts = append(parts, part)
		}
		rows = merge(d, parts...)
	} else if len(rows) >= p.limit {
		fmt.Fprintf(p.warn, "cfkeep: warning: %s %s %s reached %d rows; the smallest groups are missing\n",
			z.Name, d.Name, day.Format(dateLayout), p.limit)
	}
	key := joinKey(p.prefix, d.Name, day.Format(dateLayout)+"_"+z.Name+".jsonl")
	if len(rows) == 0 {
		fmt.Fprintf(p.log, "%s %s %s: no rows\n", z.Name, d.Name, day.Format(dateLayout))
		return nil
	}
	body, err := encodeRows(d, z, rows)
	if err != nil {
		return err
	}
	if err := p.sink.put(ctx, key, body); err != nil {
		return err
	}
	fmt.Fprintf(p.log, "%s %s %s: %d rows -> %s\n", z.Name, d.Name, day.Format(dateLayout), len(rows), key)
	return nil
}

type puller struct {
	cf     *client
	sink   sink
	prefix string
	limit  int
	log    io.Writer
	warn   io.Writer
}

func (p *puller) fetch(ctx context.Context, z zone, d dataset, from, to time.Time) ([]row, error) {
	data, err := p.cf.graphql(ctx, d.query(z, from, to, p.limit))
	if err != nil {
		return nil, err
	}
	var res struct {
		Viewer struct {
			Zones    []struct{ Rows []json.RawMessage } `json:"zones"`
			Accounts []struct{ Rows []json.RawMessage } `json:"accounts"`
		} `json:"viewer"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	scopes := res.Viewer.Zones
	if d.Scope == accountScope {
		scopes = res.Viewer.Accounts
	}
	if len(scopes) == 0 {
		return nil, errors.New("no data returned: check the token's Analytics read permissions")
	}
	out := make([]row, 0, len(scopes[0].Rows))
	for _, raw := range scopes[0].Rows {
		r, err := d.parse(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func joinKey(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p = strings.Trim(p, "/"); p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "/")
}
