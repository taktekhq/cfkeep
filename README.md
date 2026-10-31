# cfkeep

Keep your Cloudflare analytics after Cloudflare deletes them.

`cfkeep` pulls one day at a time from Cloudflare's GraphQL Analytics API and writes each day as a JSON Lines file, to a folder or to an S3-compatible bucket such as R2. Run it once a day and you keep everything: which pages people loaded, which bots crawled you, which pages ChatGPT and Claude read while answering someone.

```
$ cfkeep --out data --days 2 example.com
example.com pageloads 2026-10-29: 786 rows -> pageloads/2026-10-29_example.com.jsonl
example.com bots 2026-10-29: 205 rows -> bots/2026-10-29_example.com.jsonl
example.com ai_fetches 2026-10-29: 1921 rows -> ai_fetches/2026-10-29_example.com.jsonl
example.com daily 2026-10-29: 1 rows -> daily/2026-10-29_example.com.jsonl
...

$ head -1 data/ai_fetches/2026-10-29_example.com.jsonl
{"zone":"example.com","date":"2026-10-29","host":"example.com","path":"/","user_agent":"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot","status":200,"requests":46}
```

## Why

Cloudflare sees every request, including the visitors your JavaScript tracker misses and the bots it never sees. But on the Free plan it keeps request data for 31 days, and the dashboard can't be joined with anything else.

`cfkeep` turns that into plain files you own. Load them into PostHog, BigQuery, DuckDB or a spreadsheet and keep them as long as you like.

## Install

```sh
go install github.com/taktekhq/cfkeep@latest
```

Or download the source and run `go build`. It has no dependencies.

## Token

Create a Cloudflare API token at **My Profile > API Tokens > Create Token > Custom token** with these read permissions:

| Scope | Permission | Used for |
|---|---|---|
| Zone | Zone: Read | looking the zone up by its name |
| Zone | Analytics: Read | `bots`, `ai_fetches`, `daily` |
| Account | Account Analytics: Read | `pageloads` (Web Analytics) |

Pass it as `$CLOUDFLARE_API_TOKEN`, or put it in a file and use `--token-file`.

## Use

```sh
cfkeep example.com                          # the last 3 complete UTC days, into ./cfkeep-data
cfkeep --days 31 example.com other.org      # backfill everything Cloudflare still has
cfkeep --from 2026-10-01 --to 2026-10-15 example.com
cfkeep --datasets ai_fetches,daily example.com
cfkeep datasets                             # list the datasets and their columns
```

Each file is `<out>/<dataset>/<YYYY-MM-DD>_<zone>.jsonl`. Running a day again replaces its file, so overlapping runs are safe. A day with no rows writes no file. Network errors, HTTP 429 and 5xx answers are retried three times (after 2, 10 and 30 seconds). `cfkeep` exits 1 if any day still failed, after trying all the others.

### Into a bucket

```sh
export AWS_ACCESS_KEY_ID=...  AWS_SECRET_ACCESS_KEY=...
cfkeep --out s3://my-bucket/cloudflare \
       --s3-endpoint https://<account-id>.r2.cloudflarestorage.com example.com
```

For R2, create the S3 keys under **R2 > Manage API tokens** with Object Read & Write on the bucket. The region defaults to `auto`, which is what R2 wants. For Amazon S3, pass `--s3-region us-east-1` (or yours) and leave out `--s3-endpoint`. MinIO and other S3-compatible stores work with their own endpoint.

## Datasets

| Dataset | What it holds | Columns |
|---|---|---|
| `pageloads` | Web Analytics page loads (the beacon Cloudflare adds to your pages), by page, referrer, country and device | zone, date, host, path, referrer_host, country, device, page_loads, visits |
| `bots` | Requests from bots Cloudflare verified (search engines, AI crawlers, monitors), by bot, category and status | zone, date, host, category, user_agent, status, requests |
| `ai_fetches` | Pages AI assistants fetched while answering someone (ChatGPT-User, Claude-User, Perplexity-User), by page | zone, date, host, path, user_agent, status, requests |
| `daily` | Daily totals: requests, page views, unique visitors, bytes, cached requests and threats | zone, date, requests, page_views, uniques, bytes, cached_requests, threats |

`pageloads` needs Web Analytics turned on for the site. Every dataset works on the Free plan.

How long Cloudflare keeps each one on Free: request data (`bots`, `ai_fetches`) 31 days, `daily` about a year, Web Analytics a few months (one query can span 13 weeks).

The counts in `pageloads`, `bots` and `ai_fetches` come from Cloudflare's "adaptive" datasets, which sample busy sites and scale the counts back up. They match the Cloudflare dashboard, not your server logs to the request.

One Cloudflare query returns at most 10,000 rows. When a day reaches that, `cfkeep` fetches it again hour by hour and adds the hours up. If a single hour still reaches 10,000 rows, it prints a warning: the smallest groups of that hour are missing.

## Run it every day

A systemd user timer:

```ini
# ~/.config/systemd/user/cfkeep.service
[Service]
Type=oneshot
EnvironmentFile=%h/.config/cfkeep/env
ExecStart=%h/go/bin/cfkeep -q --days 3 --out s3://my-bucket/cloudflare example.com

# ~/.config/systemd/user/cfkeep.timer
[Timer]
OnCalendar=*-*-* 04:20:00 UTC
Persistent=true

[Install]
WantedBy=timers.target
```

`~/.config/cfkeep/env` holds `CLOUDFLARE_API_TOKEN`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `CFKEEP_S3_ENDPOINT`, readable only by you (`chmod 600`). Then `systemctl --user enable --now cfkeep.timer`.

Or cron: `20 4 * * * cfkeep -q --days 3 --out /srv/cfkeep example.com`.

`--days 3` re-pulls the last three days, so a missed run or a late Cloudflare update fixes itself the next day.

## Load the files

**PostHog.** Add a Cloudflare R2 (or S3) source in the data warehouse. Point the files URL at one dataset's folder, for example `https://<account-id>.r2.cloudflarestorage.com/my-bucket/cloudflare/ai_fetches/`, choose JSON Lines, and turn on "Combine every file into one table". Repeat for each dataset you want. Use a read-only R2 key for PostHog.

**DuckDB.**

```sql
SELECT path, sum(requests) AS fetches
FROM read_json_auto('cfkeep-data/ai_fetches/*.jsonl')
GROUP BY path ORDER BY fetches DESC LIMIT 20;
```

## License

MIT. See [LICENSE](LICENSE).
