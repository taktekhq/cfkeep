package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The GET Object example from the AWS Signature Version 4 documentation.
func TestSignV4MatchesAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	empty := sha256.Sum256(nil)
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	signV4(req, hex.EncodeToString(empty[:]), "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3", at)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestDayRange(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	days, err := dayRange("", "", 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmtDays(days); got != "2026-10-28 2026-10-29 2026-10-30" {
		t.Fatalf("last 3 days: %s", got)
	}
	days, err = dayRange("2026-09-30", "2026-10-01", 3, now)
	if err != nil || fmtDays(days) != "2026-09-30 2026-10-01" {
		t.Fatalf("range: %v %s", err, fmtDays(days))
	}
	for _, c := range [][2]string{{"2026-10-02", "2026-10-01"}, {"2026-10-01", ""}, {"2026-10-01", "2026-11-05"}, {"10/01", "2026-10-02"}} {
		if _, err := dayRange(c[0], c[1], 3, now); err == nil {
			t.Errorf("dayRange(%q, %q) should fail", c[0], c[1])
		}
	}
}

func fmtDays(ds []time.Time) string {
	s := make([]string, len(ds))
	for i, d := range ds {
		s[i] = d.Format(dateLayout)
	}
	return strings.Join(s, " ")
}

func TestQueryShape(t *testing.T) {
	z := zone{ID: "zid", Name: "example.com", AccountID: "aid"}
	day := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	pl, _ := pickDatasets("pageloads")
	q := pl[0].query(z, day, day.AddDate(0, 0, 1), 10000)
	for _, want := range []string{`accounts(filter:{accountTag:"aid"})`, `rows:rumPageloadEventsAdaptiveGroups(limit:10000`,
		`datetime_geq:"2026-10-29T00:00:00Z",datetime_lt:"2026-10-30T00:00:00Z"`, `{requestHost_like:"%.example.com"}`,
		`count sum{visits} dimensions{date requestHost requestPath refererHost countryName deviceType}`} {
		if !strings.Contains(q, want) {
			t.Errorf("pageloads query missing %s\n%s", want, q)
		}
	}
	daily, _ := pickDatasets("daily")
	q = daily[0].query(z, day, day.AddDate(0, 0, 1), 10000)
	for _, want := range []string{`zones(filter:{zoneTag:"zid"})`, `date_geq:"2026-10-29",date_leq:"2026-10-29"`,
		`sum{requests pageViews bytes cachedRequests threats} uniq{uniques}`} {
		if !strings.Contains(q, want) {
			t.Errorf("daily query missing %s\n%s", want, q)
		}
	}
	if _, err := pickDatasets("bots,nope"); err == nil {
		t.Error("unknown dataset should fail")
	}
}

// fakeCloudflare answers the zones lookup and GraphQL. A whole-day ai_fetches query returns
// exactly the row limit, so cfkeep must fetch the day again by hour and sum the hours.
func fakeCloudflare(t *testing.T, queries *[]string) *httptest.Server {
	geq := regexp.MustCompile(`datetime_geq:"([^"]+)",datetime_lt:"([^"]+)"`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad token", 403)
			return
		}
		if r.URL.Path == "/zones" {
			fmt.Fprint(w, `{"result":[{"id":"zid","name":"example.com","account":{"id":"aid"}}]}`)
			return
		}
		var body struct{ Query string }
		json.NewDecoder(r.Body).Decode(&body)
		*queries = append(*queries, body.Query)
		var rows []string
		switch {
		case strings.Contains(body.Query, "httpRequests1dGroups"):
			rows = []string{`{"sum":{"requests":10,"pageViews":4,"bytes":2048,"cachedRequests":1,"threats":0},"uniq":{"uniques":3},"dimensions":{"date":"2026-10-29"}}`}
		case strings.Contains(body.Query, `"AI Assistant"`):
			m := geq.FindStringSubmatch(body.Query)
			from, _ := time.Parse(time.RFC3339, m[1])
			to, _ := time.Parse(time.RFC3339, m[2])
			if to.Sub(from) == 24*time.Hour {
				for i := 0; i < 3; i++ { // limit is 3 in the test: a full page
					rows = append(rows, fmt.Sprintf(`{"count":1,"dimensions":{"date":"2026-10-29","clientRequestHTTPHost":"example.com","clientRequestPath":"/p%d","userAgent":"ChatGPT-User","edgeResponseStatus":200}}`, i))
				}
			} else if from.Hour() < 2 { // the same page in hours 0 and 1, another page in hour 0
				rows = append(rows, `{"count":2,"dimensions":{"date":"2026-10-29","clientRequestHTTPHost":"example.com","clientRequestPath":"/a","userAgent":"ChatGPT-User","edgeResponseStatus":200}}`)
				if from.Hour() == 0 {
					rows = append(rows, `{"count":1,"dimensions":{"date":"2026-10-29","clientRequestHTTPHost":"example.com","clientRequestPath":"/b","userAgent":"Claude-User","edgeResponseStatus":404}}`)
				}
			}
		}
		node := "zones"
		if strings.Contains(body.Query, "accounts(") {
			node = "accounts"
		}
		fmt.Fprintf(w, `{"data":{"viewer":{%q:[{"rows":[%s]}]}}}`, node, strings.Join(rows, ","))
	}))
}

func TestRunWritesFilesAndSplitsBusyDays(t *testing.T) {
	var queries []string
	srv := fakeCloudflare(t, &queries)
	defer srv.Close()
	out := t.TempDir()

	cf := newClient("tok")
	cf.api = srv.URL
	p := &puller{cf: cf, sink: dirSink{root: out}, limit: 3, log: io.Discard, warn: io.Discard}
	z, err := cf.zone(context.Background(), "Example.com")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"ai_fetches", "daily", "pageloads"} {
		d, _ := pickDatasets(name)
		if err := p.pull(context.Background(), z, d[0], day); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(queries) != 1+24+1+1 {
		t.Errorf("want 27 queries (ai_fetches day + 24 hours, daily, pageloads), got %d", len(queries))
	}

	got, err := os.ReadFile(filepath.Join(out, "ai_fetches", "2026-10-29_example.com.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"zone":"example.com","date":"2026-10-29","host":"example.com","path":"/a","user_agent":"ChatGPT-User","status":200,"requests":4}
{"zone":"example.com","date":"2026-10-29","host":"example.com","path":"/b","user_agent":"Claude-User","status":404,"requests":1}
`
	if string(got) != want {
		t.Errorf("ai_fetches file\n got %s\nwant %s", got, want)
	}
	got, _ = os.ReadFile(filepath.Join(out, "daily", "2026-10-29_example.com.jsonl"))
	if !strings.Contains(string(got), `"date":"2026-10-29","requests":10,"page_views":4,"uniques":3,"bytes":2048`) {
		t.Errorf("daily file: %s", got)
	}
	if _, err := os.Stat(filepath.Join(out, "pageloads", "2026-10-29_example.com.jsonl")); !os.IsNotExist(err) {
		t.Error("a day with no rows should not write a file")
	}
}

func TestS3SinkPutsSignedObject(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	env := map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}
	s, prefix, err := openSink("s3://bucket/cf/data", srv.URL, "", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"a":1}` + "\n")
	if err := s.put(context.Background(), joinKey(prefix, "daily", "2026-10-29_example.com.jsonl"), payload); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPut || got.URL.Path != "/bucket/cf/data/daily/2026-10-29_example.com.jsonl" {
		t.Errorf("request %s %s", got.Method, got.URL.Path)
	}
	sum := sha256.Sum256(payload)
	if got.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) || !bytes.Equal(body, payload) {
		t.Error("payload hash or body mismatch")
	}
	if a := got.Header.Get("Authorization"); !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AK/") || !strings.Contains(a, "/auto/s3/aws4_request") {
		t.Errorf("Authorization: %s", a)
	}
}

func TestOpenSinkErrors(t *testing.T) {
	none := func(string) string { return "" }
	if _, _, err := openSink("s3://bucket", "", "", none); err == nil {
		t.Error("R2 without an endpoint should fail")
	}
	if _, _, err := openSink("s3://bucket", "https://x.r2.cloudflarestorage.com", "", none); err == nil {
		t.Error("missing credentials should fail")
	}
	if s, _, err := openSink("./data", "", "", none); err != nil {
		t.Error(err)
	} else if _, ok := s.(dirSink); !ok {
		t.Error("a plain path should be a folder")
	}
}

func TestRunNeedsTokenAndZone(t *testing.T) {
	var stderr bytes.Buffer
	none := func(string) string { return "" }
	if code := run(context.Background(), nil, io.Discard, &stderr, none, time.Now); code != 2 {
		t.Errorf("no zone: exit %d", code)
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"example.com"}, io.Discard, &stderr, none, time.Now); code != 2 || !strings.Contains(stderr.String(), "no API token") {
		t.Errorf("no token: exit %d, %s", code, stderr.String())
	}
}
