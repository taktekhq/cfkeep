package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTemporaryFailuresAreRetried(t *testing.T) {
	saved := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryDelays = saved }()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case strings.Contains(r.URL.Path, "denied"):
			http.Error(w, "AccessDenied", 403)
		case calls == 1:
			http.Error(w, "InternalError", 500)
		}
	}))
	defer srv.Close()
	env := map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}
	s, _, err := openSink("s3://bucket", srv.URL, "", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.put(context.Background(), "ok.jsonl", []byte("{}\n")); err != nil || calls != 2 {
		t.Fatalf("a 500 then a 200 should succeed on the second try: err=%v calls=%d", err, calls)
	}
	calls = 10
	if err := s.put(context.Background(), "denied.jsonl", []byte("{}\n")); err == nil || calls != 11 {
		t.Fatalf("a 403 must fail at once: err=%v calls=%d", err, calls-10)
	}

	calls = 0
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "rate limited", 429)
			return
		}
		w.Write([]byte(`{"result":[{"id":"zid","name":"example.com","account":{"id":"aid"}}]}`))
	}))
	defer cf.Close()
	c := newClient("tok")
	c.api = cf.URL
	if z, err := c.zone(context.Background(), "example.com"); err != nil || z.ID != "zid" || calls != 3 {
		t.Fatalf("two 429s then a 200 should succeed: %v %+v calls=%d", err, z, calls)
	}
}
