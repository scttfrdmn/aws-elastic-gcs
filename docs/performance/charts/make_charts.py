#!/usr/bin/env python3
"""Render the performance charts in docs/performance/charts/ from benchmark results.

Reads test/benchmark/results/<campaign>/summary.json and writes static SVGs that
follow the dataviz mark specs (thin bars with 4px rounded data ends, 2px lines,
8px markers with a 2px surface ring, hairline solid grid, legend for 2+ series,
text in ink tokens, light and dark palettes via prefers-color-scheme). Every
mark carries a <title> for hover; each chart has a table twin in the guides.

usage: make_charts.py [CAMPAIGN]   (default 2026-11-us-west-2; stdlib only)
"""
import json
import os
import sys
from html import escape

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CAMPAIGN = sys.argv[1] if len(sys.argv) > 1 else "2026-11-us-west-2"
S = json.load(open(os.path.join(ROOT, "test", "benchmark", "results", CAMPAIGN, "summary.json")))
OUT = os.path.dirname(os.path.abspath(__file__))

# Reference palette (validated: 3 categorical slots, light and dark).
STYLE = """
  .surface { fill: #fcfcfb; } .ink1 { fill: #0b0b0b; } .ink2 { fill: #52514e; } .muted { fill: #898781; }
  .grid { stroke: #e1e0d9; } .axis { stroke: #c3c2b7; }
  .s1 { fill: #2a78d6; } .s2 { fill: #eb6834; } .s3 { fill: #1baf7a; }
  .l1 { stroke: #2a78d6; } .l2 { stroke: #eb6834; } .l3 { stroke: #1baf7a; }
  .ring { stroke: #fcfcfb; }
  @media (prefers-color-scheme: dark) {
    .surface { fill: #1a1a19; } .ink1 { fill: #f0efec; } .ink2 { fill: #c3c2b7; }
    .grid { stroke: #2c2c2a; } .axis { stroke: #383835; }
    .s1 { fill: #3987e5; } .s2 { fill: #d95926; } .s3 { fill: #199e70; }
    .l1 { stroke: #3987e5; } .l2 { stroke: #d95926; } .l3 { stroke: #199e70; }
    .ring { stroke: #1a1a19; }
  }
  text { font-family: system-ui, -apple-system, "Segoe UI", sans-serif; }
  .t-title { font-size: 16px; font-weight: 600; } .t-sub { font-size: 12px; }
  .t-tick { font-size: 11px; font-variant-numeric: tabular-nums; } .t-label { font-size: 12px; }
  .t-value { font-size: 12px; font-weight: 600; }
"""


class SVG:
    def __init__(self, w, h, title, subtitle, desc):
        self.w, self.h, self.parts = w, h, []
        self.parts.append(f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}" role="img" aria-labelledby="t d">')
        self.parts.append(f'<title id="t">{escape(title)}</title><desc id="d">{escape(desc)}</desc><style>{STYLE}</style>')
        self.parts.append(f'<rect class="surface" x="0" y="0" width="{w}" height="{h}" rx="8"/>')
        self.text(24, 30, title, "t-title ink1")
        for i, line in enumerate([subtitle] if isinstance(subtitle, str) else subtitle):
            self.text(24, 50 + 16 * i, line, "t-sub ink2")

    def text(self, x, y, s, cls, anchor="start"):
        self.parts.append(f'<text x="{x:.1f}" y="{y:.1f}" class="{cls}" text-anchor="{anchor}">{escape(s)}</text>')

    def line(self, x1, y1, x2, y2, cls, width=1):
        self.parts.append(f'<line x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}" class="{cls}" stroke-width="{width}"/>')

    def hbar(self, x0, y, length, thick, cls, tip):
        """Horizontal bar: square at the baseline (x0), 4px rounded data end."""
        r = min(4, thick / 2, length / 2) if length > 0 else 0
        x1 = x0 + length
        d = (f"M{x0:.1f},{y:.1f} H{x1 - r:.1f} Q{x1:.1f},{y:.1f} {x1:.1f},{y + r:.1f} "
             f"V{y + thick - r:.1f} Q{x1:.1f},{y + thick:.1f} {x1 - r:.1f},{y + thick:.1f} H{x0:.1f} Z")
        self.parts.append(f'<path d="{d}" class="{cls}"><title>{escape(tip)}</title></path>')

    def vbar(self, x, y0, height, thick, cls, tip):
        """Vertical column: square at the baseline (y0), 4px rounded data end."""
        r = min(4, thick / 2, height / 2) if height > 0 else 0
        y1 = y0 - height
        d = (f"M{x:.1f},{y0:.1f} V{y1 + r:.1f} Q{x:.1f},{y1:.1f} {x + r:.1f},{y1:.1f} "
             f"H{x + thick - r:.1f} Q{x + thick:.1f},{y1:.1f} {x + thick:.1f},{y1 + r:.1f} V{y0:.1f} Z")
        self.parts.append(f'<path d="{d}" class="{cls}"><title>{escape(tip)}</title></path>')

    def polyline(self, pts, cls):
        p = " ".join(f"{x:.1f},{y:.1f}" for x, y in pts)
        self.parts.append(f'<polyline points="{p}" fill="none" class="{cls}" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>')

    def dot(self, x, y, fill_cls, tip):
        self.parts.append(f'<circle cx="{x:.1f}" cy="{y:.1f}" r="5" class="{fill_cls} ring" stroke-width="2"><title>{escape(tip)}</title></circle>')

    def legend(self, x, y, items):
        for cls, label in items:
            self.parts.append(f'<rect x="{x}" y="{y - 9}" width="10" height="10" rx="2" class="{cls}"/>')
            self.text(x + 16, y, label, "t-label ink2")
            x += 24 + 7 * len(label)

    def save(self, name):
        self.parts.append("</svg>")
        path = os.path.join(OUT, name)
        open(path, "w").write("\n".join(self.parts) + "\n")
        print("wrote", os.path.relpath(path, ROOT))


def nice_max(v, step):
    return step * (int(v / step) + 1)


def peer_settings():
    rows = S["peer_settings"]["rows"]
    conc = S["peer_settings"]["concurrent_normal_peer"]
    items = [(r["label"], r["wall_gbps"], "s1", f'{r["label"]}: {r["wall_gbps"]:.2f} Gbps (negotiated concurrency {r["negotiated"][0]})') for r in rows]
    items.append((conc["label"], conc["aggregate_wall_gbps"], "s2",
                  f'{conc["label"]}: {conc["aggregate_wall_gbps"]:.2f} Gbps aggregate ({conc["tasks"]} tasks)'))
    w, left, right = 760, 280, 70
    top, band, thick = 92, 40, 22
    h = top + band * len(items) + 64
    svg = SVG(w, h, "The other endpoint's settings set the speed, not AWS",
              "Throughput src -> dst, 35 GB of Sentinel-2 COGs, Gbps (wall clock). Our side tuned (concurrency 32/64) in every row.",
              "Horizontal bar chart. Single tasks against a default (normal) peer run at about 1.1 Gbps with 1 or 4 DTNs; an aggressive peer 1.6; a tuned peer 4.8. Eight concurrent tasks against the normal peer reach 5.0 Gbps aggregate.")
    xmax = nice_max(max(v for _, v, _, _ in items), 1)
    scale = (w - left - right) / xmax
    for i in range(int(xmax) + 1):
        x = left + i * scale
        svg.line(x, top - 6, x, top + band * len(items) - 10, "grid")
        svg.text(x, top + band * len(items) + 8, f"{i}", "t-tick muted", "middle")
    svg.text(left + (w - left - right) / 2, top + band * len(items) + 26, "Gbps", "t-tick muted", "middle")
    for k, (label, v, cls, tip) in enumerate(items):
        y = top + k * band
        svg.text(left - 12, y + thick / 2 + 4, label, "t-label ink1", "end")
        svg.hbar(left, y, v * scale, thick, cls, tip)
        svg.text(left + v * scale + 6, y + thick / 2 + 4, f"{v:.2f}", "t-value ink1")
    svg.line(left, top - 6, left, top + band * len(items) - 10, "axis")
    svg.legend(left, h - 14, [("s1", "Single task"), ("s2", "8 concurrent tasks (aggregate)")])
    svg.save("peer-settings.svg")


def dtn_scaling():
    rows = S["dtn_scaling"]["rows"]
    conc = S["dtn_scaling"]["concurrent_tasks_8dtn"]
    w, h, left, right, top, bottom = 760, 400, 64, 150, 96, 84
    svg = SVG(w, h, "One task scales to ~4 DTNs; more need more tasks",
              ["Single task src -> dst on N c8in.2xlarge DTNs per side, Gbps. Endpoint concurrency is capped",
               "at 128 processes per task, so past ~4 DTNs each node gets too few processes."],
              "Line chart. Steady state: 9.2, 18.1, 37.2 Gbps at 1, 2, 4 DTNs. Wall clock: 6.3, 11.5, 17.4, 13.0 at 1, 2, 4, 8. Two concurrent tasks at 8 DTNs: 25.0 Gbps aggregate.")
    xs = [1, 2, 4, 8]
    pw, ph = w - left - right, h - top - bottom
    xpos = {n: left + (n - 1) / 7 * pw for n in xs}  # linear axis, 1..8
    ymax = 40
    y = lambda v: top + ph - v / ymax * ph
    for t in range(0, ymax + 1, 10):
        svg.line(left, y(t), left + pw, y(t), "grid")
        svg.text(left - 8, y(t) + 4, f"{t}", "t-tick muted", "end")
    svg.line(left, top + ph, left + pw, top + ph, "axis")
    for n in xs:
        svg.text(xpos[n], top + ph + 18, f"{n}", "t-tick muted", "middle")
    svg.text(left + pw / 2, top + ph + 38, "DTNs per side", "t-tick muted", "middle")
    steady = [(xpos[r["dtns"]], y(r["steady_gbps"]), r) for r in rows if r.get("steady_gbps")]
    wall = [(xpos[r["dtns"]], y(r["wall_gbps"]), r) for r in rows]
    svg.polyline([(a, b) for a, b, _ in steady], "l1")
    svg.polyline([(a, b) for a, b, _ in wall], "l2")
    for a, b, r in steady:
        svg.dot(a, b, "s1", f'{r["dtns"]} DTNs: {r["steady_gbps"]} Gbps steady state')
    for a, b, r in wall:
        svg.dot(a, b, "s2", f'{r["dtns"]} DTNs: {r["wall_gbps"]:.1f} Gbps wall clock ({r["bytes"] / 1e9:.0f} GB)')
    cx, cy = xpos[8], y(conc["aggregate_wall_gbps"])
    svg.dot(cx, cy, "s3", f'8 DTNs, {conc["tasks"]} concurrent tasks: {conc["aggregate_wall_gbps"]} Gbps aggregate wall clock')
    sa, sb, sr = steady[-1]
    svg.text(sa + 10, sb + 4, f'{sr["steady_gbps"]:.0f} steady', "t-value ink1")
    svg.text(cx + 10, cy + 4, f'{conc["aggregate_wall_gbps"]:.0f} with 2 tasks', "t-value ink1")
    wa, wb, wr = wall[-1]
    svg.text(wa + 10, wb + 4, f'{wr["wall_gbps"]:.0f} wall, 1 task', "t-value ink1")
    svg.legend(left, h - 14, [("s1", "Steady state (sum of DTN NICs)"), ("s2", "Wall clock, 1 task"), ("s3", "Wall clock, 2 concurrent tasks")])
    svg.save("dtn-scaling.svg")


def instance_types():
    rows = S["cpu_vendor"]["rows"]
    w, h = 760, 300
    svg = SVG(w, h, "Network limits pick the instance; AMD cores are more efficient",
              "One DTN per type, 198 GB of Sentinel-2 COGs, concurrency 32. Left: sustained Gbps. Right: vCPUs used per Gbps.",
              "Two column charts. Sustained Gbps: c8in.xlarge 6.25, m8azn.xlarge 5.0, c8in.2xlarge 9.2. vCPU per Gbps: 0.49, 0.29, 0.40.")
    panels = [("Sustained Gbps", "steady_gbps", 10, 2, "{:.2f}"), ("vCPU per Gbps (lower is better)", "vcpu_per_gbps", 0.6, 0.2, "{:.2f}")]
    pw, top, ph = 300, 90, 140
    for p, (ptitle, key, ymax, step, fmt) in enumerate(panels):
        left = 70 + p * (pw + 70)
        svg.text(left, top - 14, ptitle, "t-label ink2")
        y = lambda v: top + ph - v / ymax * ph
        t = 0.0
        while t <= ymax + 1e-9:
            svg.line(left, y(t), left + pw, y(t), "grid")
            svg.text(left - 8, y(t) + 4, f"{t:g}", "t-tick muted", "end")
            t += step
        svg.line(left, top + ph, left + pw, top + ph, "axis")
        slot = pw / len(rows)
        for i, r in enumerate(rows):
            x = left + i * slot + (slot - 24) / 2
            v = r[key]
            svg.vbar(x, top + ph, v / ymax * ph, 24, "s1", f'{r["type"]} ({r["vendor"]}, {r["cores"]} cores @ {r["ghz"]} GHz): {fmt.format(v)}; limit: {r["binding_limit"]}')
            svg.text(x + 12, y(v) - 6, fmt.format(v), "t-value ink1", "middle")
            svg.text(x + 12, top + ph + 18, r["type"], "t-tick muted", "middle")
            svg.text(x + 12, top + ph + 32, r["vendor"].split(" ")[0], "t-tick muted", "middle")
    svg.save("instance-types.svg")


def rtt():
    rows = S["rtt"]["rows"]
    tuned = [r for r in rows if r["peer"].startswith("custom") and r.get("parallelism") == 8]
    normal = [r for r in rows if r["peer"] == "normal"]
    par1 = [r for r in rows if r.get("parallelism") == 1]
    w, h, left, right, top, bottom = 760, 380, 64, 170, 96, 84
    svg = SVG(w, h, "Latency barely matters once concurrency is high",
              ["35 GB src -> dst with added round-trip time (tc netem on dst -> src traffic), Gbps wall clock.",
               "The normal peer was measured at 0 and +80 ms only; Globus raised its concurrency 4 -> 8 at +80 ms."],
              "Line chart. Tuned peer at parallelism 8: 4.8, 5.2, 4.8, 4.8 Gbps at 0, 20, 50, 80 ms. Normal peer: 1.1 at 0 ms, 1.9 at 80 ms. Tuned peer at parallelism 1 and +50 ms: 5.2.")
    xs = [0, 20, 50, 80]
    pw, ph = w - left - right, h - top - bottom
    x = lambda ms: left + ms / 80 * pw
    ymax = 6
    y = lambda v: top + ph - v / ymax * ph
    for t in range(0, ymax + 1, 2):
        svg.line(left, y(t), left + pw, y(t), "grid")
        svg.text(left - 8, y(t) + 4, f"{t}", "t-tick muted", "end")
    svg.line(left, top + ph, left + pw, top + ph, "axis")
    for ms in xs:
        svg.text(x(ms), top + ph + 18, f"+{ms} ms" if ms else "0 ms", "t-tick muted", "middle")
    svg.text(left + pw / 2, top + ph + 38, "Added round-trip time", "t-tick muted", "middle")
    svg.polyline([(x(r["added_rtt_ms"]), y(r["wall_gbps"])) for r in tuned], "l1")
    for r in tuned:
        svg.dot(x(r["added_rtt_ms"]), y(r["wall_gbps"]), "s1", f'Tuned peer, parallelism 8, +{r["added_rtt_ms"]} ms: {r["wall_gbps"]:.2f} Gbps')
    for r in normal:
        n = r.get("negotiated", ["?"])[0]
        svg.dot(x(r["added_rtt_ms"]), y(r["wall_gbps"]), "s2", f'Normal peer, +{r["added_rtt_ms"]} ms: {r["wall_gbps"]:.2f} Gbps (negotiated concurrency {n})')
    for r in par1:
        svg.dot(x(r["added_rtt_ms"]), y(r["wall_gbps"]), "s3", f'Tuned peer, parallelism 1, +{r["added_rtt_ms"]} ms: {r["wall_gbps"]:.2f} Gbps')
    lt, ln = tuned[-1], normal[-1]
    svg.text(x(lt["added_rtt_ms"]) + 10, y(lt["wall_gbps"]) + 4, "Tuned peer", "t-value ink1")
    svg.text(x(ln["added_rtt_ms"]) + 10, y(ln["wall_gbps"]) + 4, "Normal peer", "t-value ink1")
    svg.legend(left, h - 14, [("s1", "Tuned peer, parallelism 8"), ("s2", "Normal peer (Globus default)"), ("s3", "Tuned peer, parallelism 1")])
    svg.save("rtt.svg")


def network_use():
    rows = S["network_use_1dtn"]["rows"]
    # One row per setting: prefer the run with the control node out of the data path.
    pick = {}
    for r in rows:
        if r["setting"] not in pick or not r["control_in_path"]:
            pick[r["setting"]] = r
    items = list(pick.values())
    w, left, right = 760, 230, 70
    top, band, thick = 92, 44, 24
    h = top + band * len(items) + 66
    svg = SVG(w, h, "Concurrency sets per-DTN throughput",
              "Steady-state Gbps on one c8in.2xlarge DTN per side, 198 GB of Sentinel-2 COGs, both ends at the same setting.",
              "Horizontal bar chart. Steady-state throughput per DTN: normal (concurrency 4) 0.35 Gbps, aggressive (8) 1.1, custom (32) 9.2.")
    xmax = 10
    scale = (w - left - right) / xmax
    for i in range(0, xmax + 1, 2):
        x = left + i * scale
        svg.line(x, top - 6, x, top + band * len(items) - 10, "grid")
        svg.text(x, top + band * len(items) + 8, f"{i}", "t-tick muted", "middle")
    svg.text(left + (w - left - right) / 2, top + band * len(items) + 26, "Gbps per DTN (steady state)", "t-tick muted", "middle")
    for k, r in enumerate(items):
        y = top + k * band
        v = r["steady_gbps_per_dtn"]
        note = " (control node in path)" if r["control_in_path"] else ""
        svg.text(left - 12, y + thick / 2 - 2, r["setting"], "t-label ink1", "end")
        svg.text(left - 12, y + thick / 2 + 12, f'concurrency {r["concurrency"]}', "t-tick muted", "end")
        svg.hbar(left, y, v * scale, thick, "s1", f'{r["setting"]}, concurrency {r["concurrency"]}: {v} Gbps per DTN{note}')
        svg.text(left + v * scale + 6, y + thick / 2 + 4, f"{v:g}", "t-value ink1")
    svg.line(left, top - 6, left, top + band * len(items) - 10, "axis")
    if any(r["control_in_path"] for r in items):
        svg.text(24, h - 14, "normal was measured with the control node still in the data path; the others without it.", "t-tick muted")
    svg.save("network-use.svg")


def scale_out():
    rows = S["scale_out_timing"]["rows"]
    rows = sorted(rows, key=lambda r: (r["dns_gated"], r["deployment"], r["step"]))
    w, left, right = 760, 170, 70
    top, band, thick, gap = 112, 32, 18, 28
    groups = len({r["dns_gated"] for r in rows})
    bottom_y = top + band * len(rows) + gap * (groups - 1)
    h = bottom_y + 64
    svg = SVG(w, h, "A warm DTN serves in about 1-2.5 minutes",
              ["Seconds from SetDesiredCapacity on the warm pool. Bars: until the ASG reports InService, then until the",
               "node is active in GCS. Since #72, InService waits for endpoint DNS, so it is true readiness."],
              "Stacked horizontal bars of warm-pool scale-out time. Before #72: 50-129 s to GCS active. With the DNS gate: 111-148 s.")
    xmax = nice_max(max(r["to_gcs_active_s"] for r in rows), 30)
    scale = (w - left - right) / xmax
    for t in range(0, xmax + 1, 30):
        x = left + t * scale
        svg.line(x, top - 6, x, bottom_y - 10, "grid")
        svg.text(x, bottom_y + 8, f"{t}", "t-tick muted", "middle")
    svg.text(left + (w - left - right) / 2, bottom_y + 26, "seconds", "t-tick muted", "middle")
    prev, y = None, top - band
    for r in rows:
        y += band
        if r["dns_gated"] != prev:
            if prev is not None:
                y += gap
            svg.text(24, y - 10, "With DNS gate (#72)" if r["dns_gated"] else "Before DNS gate", "t-label ink2")
            prev = r["dns_gated"]
        a, b = r["to_inservice_s"], r["to_gcs_active_s"]
        svg.text(left - 12, y + thick / 2 + 4, f'{r["deployment"]} {r["step"]}', "t-label ink1", "end")
        # Segment 1 is square on both ends (it meets segment 2); 2px surface gap between segments.
        tip = f'{r["deployment"]} {r["step"]}: InService at {a} s'
        svg.parts.append(f'<rect x="{left:.1f}" y="{y:.1f}" width="{a * scale - 1:.1f}" height="{thick}" class="s1"><title>{escape(tip)}</title></rect>')
        svg.hbar(left + a * scale + 1, y, (b - a) * scale - 1, thick, "s2", f'{r["deployment"]} {r["step"]}: GCS active at {b} s')
        svg.text(left + b * scale + 6, y + thick / 2 + 4, f"{b} s", "t-value ink1")
    svg.line(left, top - 6, left, bottom_y - 10, "axis")
    svg.legend(left, h - 14, [("s1", "Until ASG InService"), ("s2", "Until node active in GCS")])
    svg.save("scale-out.svg")


if __name__ == "__main__":
    peer_settings()
    dtn_scaling()
    instance_types()
    rtt()
    network_use()
    scale_out()
