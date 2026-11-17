"""Prints "calls_per_s p50_ms p99_ms errors" from a ghz JSON report on stdin.

Only OK calls count towards the rate. Calls that ghz itself cut off when the
run's time is up ("client connection is closing", "use of closed network
connection") are not errors.
"""

import json
import sys

r = json.load(sys.stdin)
ok = (r.get("statusCodeDistribution") or {}).get("OK", 0)
lat = {d["percentage"]: d["latency"] / 1e6 for d in r.get("latencyDistribution") or []}
shutdown = ("client connection is closing", "use of closed network connection")
errors = sum(n for msg, n in (r.get("errorDistribution") or {}).items() if not any(s in msg for s in shutdown))
print(f"{ok / (r['total'] / 1e9):.2f} {lat.get(50, 0):.3f} {lat.get(99, 0):.3f} {errors}")
