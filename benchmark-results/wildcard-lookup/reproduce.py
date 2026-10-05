import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.request


parser = argparse.ArgumentParser(description="LookupSubjects PostgreSQL benchmark")
parser.add_argument("--upstream", type=Path, required=True)
parser.add_argument("--final", type=Path, required=True)
parser.add_argument("--first", type=Path)
parser.add_argument("--client", type=Path, required=True)
parser.add_argument("--database-uri", required=True)
parser.add_argument("--output", type=Path, required=True)
parser.add_argument("--seconds", type=int, default=6)
parser.add_argument("--repeats", type=int, choices=(1, 2, 3), default=3)
args = parser.parse_args()
if args.seconds < 1:
    parser.error("--seconds must be positive")
variants = {name: path.resolve() for name, path in
            (("upstream", args.upstream), ("first", args.first), ("final", args.final))
            if path is not None}
client = args.client.resolve()
root = args.output.resolve()
root.mkdir(parents=True, exist_ok=False)
server_args = [
    "serve", "--datastore-engine", "postgres",
    "--datastore-conn-uri", args.database_uri,
    "--grpc-addr", "127.0.0.1:55051",
    "--grpc-preshared-key", "local-benchmark-key",
    "--metrics-addr", "127.0.0.1:59090",
    "--telemetry-endpoint=", "--skip-release-check", "--log-level", "warn",
    "--dispatch-cache-max-cost", "128MiB",
    "--dispatch-cluster-cache-max-cost", "128MiB",
]


@contextmanager
def running_server(variant, label):
    with (root / f"{label}.log").open("w") as log:
        server = subprocess.Popen(
            [str(variants[variant]), *server_args], cwd=root, stdout=log, stderr=log,
            env=dict(os.environ, GOMAXPROCS="4", GOMEMLIMIT="2GiB"))
        try:
            for _ in range(200):
                if server.poll() is not None:
                    raise RuntimeError(f"Server exited; see {log.name}")
                try:
                    with urllib.request.urlopen("http://127.0.0.1:59090/metrics", timeout=1):
                        break
                except OSError:
                    time.sleep(0.1)
            else:
                raise RuntimeError("Server startup timed out")
            yield
        finally:
            server.terminate()
            try:
                server.wait(timeout=12)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait()
            time.sleep(0.2)


def run_client(*flags):
    result = subprocess.run(
        [str(client), *flags], cwd=root, capture_output=True, text=True,
        env=dict(os.environ, GOMAXPROCS="2"), timeout=180)
    if result.returncode:
        raise RuntimeError(f"Client failed:\n{result.stdout}\n{result.stderr}")
    return result.stdout


with running_server("final", "seed"):
    print(run_client("-action", "seed"), end="", flush=True)
for variant in variants:
    with running_server(variant, f"verify-{variant}"):
        print(variant, run_client("-action", "verify"), end="", flush=True)

orders = [["upstream", "first", "final"], ["final", "first", "upstream"],
          ["first", "upstream", "final"]]
cases = [("direct5000", "churn"), ("direct5000", "snapshot"), ("concrete", "churn")]
with (root / "measurements.jsonl").open("w") as out:
    for repeat, order in enumerate(orders[:args.repeats], 1):
        for variant in order:
            if variant not in variants:
                continue
            for scenario, mode in cases:
                if variant == "first" and scenario == "concrete":
                    continue
                label = f"{repeat}-{variant}-{scenario}-{mode}"
                with running_server(variant, label):
                    record = json.loads(run_client(
                        "-scenario", scenario, "-mode", mode, "-concurrency", "4",
                        "-duration", f"{args.seconds}s", "-rps", "10"))
                    record.update(repeat=repeat, variant=variant)
                    out.write(json.dumps(record) + "\n")
                    out.flush()
                    allocated = record["metrics_delta"]["go_memstats_alloc_bytes_total"]
                    allocated /= record["successes"]
                    print(f"{label}: p95 {record['p95_ms']:.2f} ms, "
                          f"{allocated / 1e6:.2f} MB/lookup", flush=True)
