"""Turns results/raw.csv into Markdown tables: the median of each case's runs."""

import csv
import statistics
import sys
from collections import defaultdict

SERVERS = ["go", "cairo", "fpm", "frankenphp", "roadrunner", "swoole"]
TITLES = {
    "go": "Go net/http",
    "cairo": "Cairo",
    "fpm": "nginx + PHP-FPM",
    "frankenphp": "FrankenPHP (worker)",
    "roadrunner": "RoadRunner",
    "swoole": "Swoole",
}

runs = defaultdict(list)
memory = {}
order = []
with open(sys.argv[1]) as f:
    for row in csv.DictReader(f):
        if row["case"] == "memory-after-sleep":
            memory[row["server"]] = row["rps"]
            continue
        if not row["rps"]:
            continue
        key = (row["server"], row["case"])
        runs[key].append(row)
        if row["case"] not in order:
            order.append(row["case"])

servers = [s for s in SERVERS if any(k[0] == s for k in runs)]


def cell(server, case):
    rows = runs.get((server, case))
    if not rows:
        return "–"
    rps = statistics.median(float(r["rps"]) for r in rows)
    p99 = statistics.median(float(r["p99_ms"]) for r in rows)
    errors = sum(int(float(r["errors"] or 0)) for r in rows)
    text = f"{rps:,.0f} ({p99:,.1f} ms)"
    return text + (f" ⚠ {errors} errors" if errors else "")


print("Requests per second, median of the runs, with the p99 latency in brackets.\n")
print("| Case | " + " | ".join(TITLES[s] for s in servers) + " |")
print("|---|" + "---:|" * len(servers))
for case in order:
    print(f"| {case} | " + " | ".join(cell(s, case) for s in servers) + " |")
if memory:
    print("\nResident memory after the `sleep` case (MiB):\n")
    print("| " + " | ".join(TITLES[s] for s in servers if s in memory) + " |")
    print("|" + "---:|" * len(memory))
    print("| " + " | ".join(memory[s] for s in servers if s in memory) + " |")
