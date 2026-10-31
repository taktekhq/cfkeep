package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type scope int

const (
	zoneScope scope = iota
	accountScope
)

// field maps a GraphQL field to an output column. A metric's GraphQL name is a path: "count",
// "sum.visits" or "uniq.uniques".
type field struct{ gql, col string }

type dataset struct {
	Name, About string
	Scope       scope
	Node        string
	Filter      func(z zone) string // extra GraphQL filter fields for this dataset
	Dims        []field
	Metrics     []field
	Hourly      bool // the node takes datetime filters and its metrics can be summed over hours
}

// datasets lists what cfkeep pulls. Every one works on the Cloudflare Free plan. Cloudflare keeps
// request data (bots, ai_fetches) for 31 days on Free, Web Analytics longer, daily totals for a year.
var datasets = []dataset{
	{
		Name:  "pageloads",
		About: "Web Analytics page loads (the beacon Cloudflare adds), by page, referrer, country and device.",
		Scope: accountScope,
		Node:  "rumPageloadEventsAdaptiveGroups",
		Filter: func(z zone) string {
			return fmt.Sprintf("OR:[{requestHost:%s},{requestHost_like:%s}]", gql(z.Name), gql("%."+z.Name))
		},
		Dims: []field{{"date", "date"}, {"requestHost", "host"}, {"requestPath", "path"},
			{"refererHost", "referrer_host"}, {"countryName", "country"}, {"deviceType", "device"}},
		Metrics: []field{{"count", "page_loads"}, {"sum.visits", "visits"}},
		Hourly:  true,
	},
	{
		Name:   "bots",
		About:  "Requests from bots Cloudflare verified (search engines, AI crawlers, monitors), by bot, category and status.",
		Node:   "httpRequestsAdaptiveGroups",
		Filter: func(zone) string { return `verifiedBotCategory_neq:""` },
		Dims: []field{{"date", "date"}, {"clientRequestHTTPHost", "host"}, {"verifiedBotCategory", "category"},
			{"userAgent", "user_agent"}, {"edgeResponseStatus", "status"}},
		Metrics: []field{{"count", "requests"}},
		Hourly:  true,
	},
	{
		Name:   "ai_fetches",
		About:  "Pages AI assistants fetched while answering someone (ChatGPT-User, Claude-User, Perplexity-User), by page.",
		Node:   "httpRequestsAdaptiveGroups",
		Filter: func(zone) string { return `verifiedBotCategory:"AI Assistant"` },
		Dims: []field{{"date", "date"}, {"clientRequestHTTPHost", "host"}, {"clientRequestPath", "path"},
			{"userAgent", "user_agent"}, {"edgeResponseStatus", "status"}},
		Metrics: []field{{"count", "requests"}},
		Hourly:  true,
	},
	{
		Name:  "daily",
		About: "Daily totals for the zone: requests, page views, unique visitors, bytes, cached requests and threats.",
		Node:  "httpRequests1dGroups",
		Dims:  []field{{"date", "date"}},
		Metrics: []field{{"sum.requests", "requests"}, {"sum.pageViews", "page_views"}, {"uniq.uniques", "uniques"},
			{"sum.bytes", "bytes"}, {"sum.cachedRequests", "cached_requests"}, {"sum.threats", "threats"}},
	},
}

func pickDatasets(list string) ([]dataset, error) {
	if strings.TrimSpace(list) == "" {
		return datasets, nil
	}
	var out []dataset
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		found := false
		for _, d := range datasets {
			if d.Name == name {
				out = append(out, d)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown dataset %q (run: cfkeep datasets)", name)
		}
	}
	return out, nil
}

func printDatasets(w io.Writer) {
	for _, d := range datasets {
		cols := []string{"zone"}
		for _, f := range append(append([]field{}, d.Dims...), d.Metrics...) {
			cols = append(cols, f.col)
		}
		fmt.Fprintf(w, "%s\n  %s\n  columns: %s\n\n", d.Name, d.About, strings.Join(cols, ", "))
	}
}

// gql quotes a string for a GraphQL query. JSON string syntax is valid GraphQL string syntax.
func gql(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// query builds the GraphQL query for [from, to).
func (d dataset) query(z zone, from, to time.Time, limit int) string {
	scopeNode, tag := "zones", "zoneTag:"+gql(z.ID)
	if d.Scope == accountScope {
		scopeNode, tag = "accounts", "accountTag:"+gql(z.AccountID)
	}
	var filter string
	if d.Hourly {
		filter = fmt.Sprintf("datetime_geq:%s,datetime_lt:%s", gql(from.Format(time.RFC3339)), gql(to.Format(time.RFC3339)))
	} else {
		filter = fmt.Sprintf("date_geq:%s,date_leq:%s", gql(from.Format(dateLayout)), gql(to.AddDate(0, 0, -1).Format(dateLayout)))
	}
	if d.Filter != nil {
		filter += "," + d.Filter(z)
	}
	return fmt.Sprintf("{viewer{%s(filter:{%s}){rows:%s(limit:%d,filter:{%s}){%s}}}}",
		scopeNode, tag, d.Node, limit, filter, d.selection())
}

func (d dataset) selection() string {
	var parts []string
	groups := map[string][]string{}
	var order []string
	for _, m := range d.Metrics {
		top, sub, ok := strings.Cut(m.gql, ".")
		if !ok {
			parts = append(parts, top)
			continue
		}
		if _, seen := groups[top]; !seen {
			order = append(order, top)
		}
		groups[top] = append(groups[top], sub)
	}
	for _, top := range order {
		parts = append(parts, top+"{"+strings.Join(groups[top], " ")+"}")
	}
	dims := make([]string, len(d.Dims))
	for i, f := range d.Dims {
		dims[i] = f.gql
	}
	return strings.Join(parts, " ") + " dimensions{" + strings.Join(dims, " ") + "}"
}

type row struct {
	dims    []any
	metrics []float64
}

func (d dataset) parse(raw json.RawMessage) (row, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return row{}, err
	}
	r := row{dims: make([]any, len(d.Dims)), metrics: make([]float64, len(d.Metrics))}
	dims, _ := m["dimensions"].(map[string]any)
	for i, f := range d.Dims {
		r.dims[i] = dims[f.gql]
	}
	for i, f := range d.Metrics {
		var v any = m
		for _, k := range strings.Split(f.gql, ".") {
			obj, _ := v.(map[string]any)
			v = obj[k]
		}
		if n, ok := v.(json.Number); ok {
			x, err := n.Float64()
			if err != nil {
				return row{}, err
			}
			r.metrics[i] = x
		}
	}
	return r, nil
}

func (r row) key() string {
	b, _ := json.Marshal(r.dims)
	return string(b)
}

// merge sums rows that share the same dimensions, for a day fetched hour by hour.
func merge(d dataset, parts ...[]row) []row {
	index := map[string]int{}
	var out []row
	for _, part := range parts {
		for _, r := range part {
			k := r.key()
			i, ok := index[k]
			if !ok {
				index[k] = len(out)
				out = append(out, row{dims: r.dims, metrics: append([]float64(nil), r.metrics...)})
				continue
			}
			for j, v := range r.metrics {
				out[i].metrics[j] += v
			}
		}
	}
	return out
}

// encodeRows writes one JSON object per line, biggest first, columns in a fixed order.
func encodeRows(d dataset, z zone, rows []row) ([]byte, error) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].metrics[0] != rows[j].metrics[0] {
			return rows[i].metrics[0] > rows[j].metrics[0]
		}
		return rows[i].key() < rows[j].key()
	})
	var buf bytes.Buffer
	for _, r := range rows {
		buf.WriteString(`{"zone":`)
		buf.WriteString(gql(z.Name))
		for i, f := range d.Dims {
			b, err := json.Marshal(r.dims[i])
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&buf, ",%s:%s", gql(f.col), b)
		}
		for i, f := range d.Metrics {
			fmt.Fprintf(&buf, ",%s:%s", gql(f.col), number(r.metrics[i]))
		}
		buf.WriteString("}\n")
	}
	return buf.Bytes(), nil
}

func number(x float64) string {
	if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatFloat(x, 'f', -1, 64)
}
