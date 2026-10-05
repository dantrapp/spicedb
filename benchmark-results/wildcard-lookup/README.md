# LookupSubjects wildcard exclusion benchmark

`LookupSubjects` resolves `public - banned`, where `public` is `user:*` and `banned` contains 5,000 users. The baseline copies the growing exclusion list for each removed user. Batching those removals reduces the exclusion construction from quadratic to linear work.

The measurements use the public gRPC API, the `cmd/spicedb` server, and PostgreSQL 16.11. All run on one Apple M4 with 16 GiB RAM and macOS 15.6.1. Both server builds use Go 1.26.8, `GOMAXPROCS=4`, `GOMEMLIMIT=2GiB`, and enabled 128 MiB dispatch caches. The load generator uses `GOMAXPROCS=2` and four request workers.

| Workload | Baseline p95, ms | Patched p95, ms | Baseline MB allocated/lookup | Patched MB allocated/lookup |
| --- | ---: | ---: | ---: | ---: |
| 5,000 exclusions, changing revisions | 88.73 (75.51–92.99) | 11.43 (9.69–13.60) | 111.456 | 3.629 |
| 5,000 exclusions, cached snapshot | 7.09 (6.53–8.93) | 7.25 (6.59–7.60) | 1.351 | 1.350 |
| 1,000 concrete members minus 500 users | 6.74 (5.92–9.64) | 6.83 (6.81–10.13) | 1.244 | 1.219 |

Each cell reports the median across three runs; latency ranges show the minimum and maximum per-run p95. Each run schedules 60 requests at 10 requests/second against a new server process. Latency includes client queueing and receiving the complete response, with result validation outside the timer. Every lookup checks exact subject IDs and permissionships. The recorded 1,440 lookups, including the intermediate revision, had no lookup or write errors.

Changing-revision cases use fully consistent reads and alternate a relationship addition/deletion on a separate document every 50 ms. Snapshot cases use the seed revision, no writes, and 16 warmed documents. Dispatch cache hit rates were approximately 0.6% and 100%, respectively. The concrete-subject control also uses changing revisions. Allocation is the server's total allocated-byte delta divided by successful lookups, including concurrent writes and background work. MB means 1,000,000 bytes.

These are synthetic graphs on a shared local machine. Small latency differences in the controls vary between runs. The results do not measure a multi-node deployment or customer traffic.

## Files and revisions

- `client.go`: schema, fixture seeding, exact-result verification, load generation, and metric capture.
- `reproduce.py`: starts a new server per case and alternates revision order.
- `measurements.jsonl`: unmodified per-run records, including the intermediate patch (`first`).
- `summary.json`: medians and ranges; `summarize.py` regenerates this report.
- `provenance.json`: compiler, configuration, metric definitions, and source revisions.

Baseline: `dbc16016e92987c531658eae0770261c77434adf`. Intermediate patch: `d28fc7d6565f58db594598b8f0a100228183e24e`. Measured final patch: `cd270368128a0e412cc500d993a41d95643a0ec1`. Submitted commit: `1de38724b0e75e5236a5b526fcab2426e23410d7`; its Go source and tests are identical to the measured final patch.

## Reproduce

Requires Git, Python 3, Go 1.26.8, and PostgreSQL 16 binaries (`initdb`, `pg_ctl`, `createdb`) on `PATH`. Ports 55432, 55051, and 59090 must be available. The commands create a dedicated local database with trust authentication restricted to loopback. Run in Bash as a non-root user.

```bash
git clone --branch benchmarks/wildcard-lookup-evidence https://github.com/dantrapp/spicedb.git spicedb-bench
cd spicedb-bench
repo=$PWD
evidence="$repo/benchmark-results/wildcard-lookup"
run_dir=$(mktemp -d)
export GOTOOLCHAIN=go1.26.8

git worktree add --detach "$run_dir/upstream" dbc16016e92987c531658eae0770261c77434adf
git worktree add --detach "$run_dir/first" d28fc7d6565f58db594598b8f0a100228183e24e
git worktree add --detach "$run_dir/final" 1de38724b0e75e5236a5b526fcab2426e23410d7
for variant in upstream first final; do
  (cd "$run_dir/$variant" && go build -tags=memoryprotection -ldflags=-checklinkname=0 -o "$run_dir/spicedb-$variant" ./cmd/spicedb)
done
(cd "$run_dir/final" && go build -o "$run_dir/client" "$evidence/client.go")

initdb -D "$run_dir/pgdata" -U bench -A trust --no-locale
cat >> "$run_dir/pgdata/postgresql.conf" <<'CONF'
listen_addresses = '127.0.0.1'
port = 55432
shared_buffers = '128MB'
max_connections = 60
track_commit_timestamp = on
CONF
pg_ctl -D "$run_dir/pgdata" -l "$run_dir/postgres.log" start
trap 'pg_ctl -D "$run_dir/pgdata" -m fast stop' EXIT
createdb -h 127.0.0.1 -p 55432 -U bench spicedb_bench
database_uri='postgres://bench@127.0.0.1:55432/spicedb_bench?sslmode=disable'
"$run_dir/spicedb-final" migrate head --datastore-engine postgres --datastore-conn-uri "$database_uri"

python3 "$evidence/reproduce.py" \
  --upstream "$run_dir/spicedb-upstream" --first "$run_dir/spicedb-first" \
  --final "$run_dir/spicedb-final" --client "$run_dir/client" \
  --database-uri "$database_uri" --output "$run_dir/results"
python3 "$evidence/summarize.py" "$run_dir/results/measurements.jsonl"
```

The output directory must not already exist. The runner writes the fixture through the public API, verifies all 96 documents on each revision, then runs the measurements. `--first` is optional; omitting it compares only baseline and final. Use `--repeats 1 --seconds 1` for a short harness check. The seeded database contains 280,416 relationships across six graph shapes; the timed cases above use three of them.

The PR also includes in-process benchmarks:

```bash
go test -tags=memoryprotection -ldflags=-checklinkname=0 -run '^$' -bench '^BenchmarkSubjectSetSubtractAll$' -benchmem ./internal/datasets
go test -tags=memoryprotection -ldflags=-checklinkname=0 -run '^$' -bench '^BenchmarkLookupSubjectsWildcardExclusion$' -benchmem ./pkg/query/benchmarks
```
