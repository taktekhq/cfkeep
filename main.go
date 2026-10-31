// cfkeep keeps Cloudflare analytics after Cloudflare drops them. It pulls one day at a time from the
// GraphQL Analytics API (Web Analytics page loads, verified bots, AI assistant fetches, daily totals)
// and writes each day as a JSON Lines file to a folder or an S3-compatible bucket such as R2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

var version = "0.1.0"

const usage = `cfkeep: keep Cloudflare analytics as daily JSON Lines files.

Usage:
  cfkeep [flags] ZONE...   pull the datasets for each zone (a domain name, e.g. example.com)
  cfkeep datasets          list the datasets and their columns
  cfkeep version

Flags:
  --out DIR|s3://BUCKET/PREFIX   where the files go (default ./cfkeep-data)
  --datasets LIST                comma-separated datasets (default: all)
  --days N                       the last N complete UTC days, ending yesterday (default 3)
  --from DATE --to DATE          an inclusive range of UTC days (YYYY-MM-DD) instead of --days
  --token-file PATH              read the Cloudflare API token from a file
                                 (default: $CLOUDFLARE_API_TOKEN, then $CF_API_TOKEN)
  --s3-endpoint URL              S3 endpoint ($CFKEEP_S3_ENDPOINT), e.g.
                                 https://<account-id>.r2.cloudflarestorage.com
  --s3-region REGION             S3 region ($CFKEEP_S3_REGION, default "auto" for R2)
  -q                             print only warnings and errors

S3 credentials come from $AWS_ACCESS_KEY_ID and $AWS_SECRET_ACCESS_KEY.
Files are written to <out>/<dataset>/<YYYY-MM-DD>_<zone>.jsonl. Running a day again replaces its file.
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv, time.Now))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, now func() time.Time) int {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Fprintln(stdout, version)
			return 0
		case "datasets":
			printDatasets(stdout)
			return 0
		}
	}

	fs := flag.NewFlagSet("cfkeep", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	out := fs.String("out", "cfkeep-data", "")
	names := fs.String("datasets", "", "")
	days := fs.Int("days", 3, "")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	tokenFile := fs.String("token-file", "", "")
	endpoint := fs.String("s3-endpoint", getenv("CFKEEP_S3_ENDPOINT"), "")
	region := fs.String("s3-region", getenv("CFKEEP_S3_REGION"), "")
	quiet := fs.Bool("q", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	zones := fs.Args()
	if len(zones) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	fail := func(err error) int { fmt.Fprintln(stderr, "cfkeep:", err); return 2 }
	sets, err := pickDatasets(*names)
	if err != nil {
		return fail(err)
	}
	dates, err := dayRange(*from, *to, *days, now().UTC())
	if err != nil {
		return fail(err)
	}
	token, err := readToken(*tokenFile, getenv)
	if err != nil {
		return fail(err)
	}
	dest, prefix, err := openSink(*out, *endpoint, *region, getenv)
	if err != nil {
		return fail(err)
	}

	logw := stdout
	if *quiet {
		logw = io.Discard
	}
	p := &puller{cf: newClient(token), sink: dest, prefix: prefix, limit: rowLimit, log: logw, warn: stderr}
	failed := 0
	for _, name := range zones {
		z, err := p.cf.zone(ctx, name)
		if err != nil {
			fmt.Fprintf(stderr, "cfkeep: %s: %v\n", name, err)
			failed++
			continue
		}
		for _, d := range sets {
			for _, day := range dates {
				if err := p.pull(ctx, z, d, day); err != nil {
					fmt.Fprintf(stderr, "cfkeep: %s %s %s: %v\n", z.Name, d.Name, day.Format(dateLayout), err)
					failed++
				}
			}
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

const dateLayout = "2006-01-02"

// dayRange returns the UTC days to pull: --from/--to when given, else the last n complete days.
func dayRange(from, to string, n int, now time.Time) ([]time.Time, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var start, end time.Time
	switch {
	case from == "" && to == "":
		if n < 1 {
			return nil, errors.New("--days must be at least 1")
		}
		end = today.AddDate(0, 0, -1)
		start = end.AddDate(0, 0, -(n - 1))
	case from == "" || to == "":
		return nil, errors.New("--from and --to go together")
	default:
		var err error
		if start, err = time.Parse(dateLayout, from); err != nil {
			return nil, fmt.Errorf("--from: %w", err)
		}
		if end, err = time.Parse(dateLayout, to); err != nil {
			return nil, fmt.Errorf("--to: %w", err)
		}
		if end.Before(start) {
			return nil, errors.New("--to is before --from")
		}
		if end.After(today) {
			return nil, errors.New("--to is in the future")
		}
	}
	var out []time.Time
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out, nil
}

func readToken(file string, getenv func(string) string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("%s is empty", file)
	}
	for _, k := range []string{"CLOUDFLARE_API_TOKEN", "CF_API_TOKEN"} {
		if t := strings.TrimSpace(getenv(k)); t != "" {
			return t, nil
		}
	}
	return "", errors.New("no API token: set $CLOUDFLARE_API_TOKEN or pass --token-file")
}
