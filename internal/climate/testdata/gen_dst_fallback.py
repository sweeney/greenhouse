# Generates dst_fallback_2026-10-25.json, the autumn clock-change fixture for
# issue #36 (the greenhouse port of countinghouse#49).
#
# Synthetic: one appliance probe reading temperature_c once a minute, a slow
# strictly increasing ramp from -18.00 degC, 2026-10-24T23:00Z (00:00 BST) ..
# 2026-10-25T03:00Z (03:00 GMT), so no two buckets share a value. No household data.
#
# "flux_rows" reproduces what prod Influx returned for the location-aware query
# greenhouse used to issue:
#   aggregateWindow(every: 15m, fn: mean, timeSrc: "_start",
#                   location: timezone.location(name: "Europe/London"), createEmpty: true)
# Windows are laid on LOCAL wall clock, so the 01:45 BST window (00:45Z) runs to
# the next wall time that happens only once, 02:00 GMT (02:00Z): 75 minutes, and
# nothing is stamped inside the repeated hour 01:00Z-01:45Z. The layout is
# RECONSTRUCTED from the observed /series output (prod, 2026-10-26: every probe
# null for 01:00Z-01:45Z) and from countinghouse#49's 30m capture of the same
# night, not captured verbatim from Influx.
#
# Run from the repo root:
#   python3 internal/climate/testdata/gen_dst_fallback.py \
#     > internal/climate/testdata/dst_fallback_2026-10-25.json
import json, sys
from datetime import datetime, timedelta, timezone

Z = timezone.utc
start = datetime(2026, 10, 24, 23, 0, tzinfo=Z)
stop = datetime(2026, 10, 25, 3, 0, tzinfo=Z)
step = timedelta(minutes=15)

readings = []
t, i = start, 0
while t < stop:
    readings.append((t, round(-18.0 + 0.01 * i, 6)))
    t += timedelta(minutes=1)
    i += 1

stretch_lo = datetime(2026, 10, 25, 0, 45, tzinfo=Z)  # 01:45 BST
stretch_hi = datetime(2026, 10, 25, 2, 0, tzinfo=Z)   # 02:00 GMT
windows = []
w = start
while w < stretch_lo:
    windows.append((w, w + step))
    w += step
windows.append((stretch_lo, stretch_hi))
w = stretch_hi
while w < stop:
    windows.append((w, w + step))
    w += step

iso = lambda d: d.strftime("%Y-%m-%dT%H:%M:%SZ")
rows = []
for ws, we in windows:
    inside = [v for (a, v) in readings if ws <= a < we]
    rows.append({"device_id": "probe_a", "field": "temperature_c", "time": iso(ws),
                 "value": round(sum(inside) / len(inside), 6)})

json.dump({
    "description": "One appliance probe across the Europe/London autumn clock change "
                   "(2026-10-25 02:00 BST -> 01:00 GMT). Synthetic ramp; window layout "
                   "reproduces prod Influx's location-aware 15m aggregateWindow.",
    "window": {"from": iso(start), "to": iso(stop)},
    "interval": "15m",
    "device_id": "probe_a",
    "readings": [{"time": iso(a), "temperature_c": v} for a, v in readings],
    "flux_rows": rows,
}, sys.stdout, indent=1)
sys.stdout.write("\n")
