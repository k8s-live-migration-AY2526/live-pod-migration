#!/usr/bin/env python3
"""
Plot cache-preservation benchmark results.

Produces a two-panel figure (hit_rate + p99 latency over time) for the
eviction and migration trials, with the blackout window shaded on each.

Usage:
    python3 plot_results.py [--out figure.pdf]

Expects evict_stats.csv, evict_meta.txt, migrate_stats.csv, migrate_meta.txt
in the same directory as this script.
"""

import argparse, csv, math, os
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
from matplotlib.lines import Line2D

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

COLORS = {
    'evict':   '#e05c5c',   # red
    'migrate': '#4a90d9',   # blue
}
ALPHA_SHADE  = 0.20
SMOOTH_WINDOW = 8   # seconds rolling average for noisy series

# ---------------------------------------------------------------------------
# Data loading
# ---------------------------------------------------------------------------

def load_trial(script_dir, strategy):
    stats_path = os.path.join(script_dir, f'{strategy}_stats.csv')
    meta_path  = os.path.join(script_dir, f'{strategy}_meta.txt')

    with open(meta_path) as f:
        meta = dict(line.strip().split('=', 1) for line in f if '=' in line)
    event_ms = int(meta['EVENT_TIMESTAMP'])

    times, hit_rates, p50s, statuses = [], [], [], []
    ok_times = []

    with open(stats_path) as f:
        for row in csv.DictReader(f):
            ts_ms  = int(row['timestamp_ms'])
            t      = (ts_ms - event_ms) / 1000.0
            status = row['status']

            times.append(t)
            statuses.append(status)

            hit_rate = float(row['hit_rate']) if row['hit_rate'] not in ('N/A', '') else None
            p50      = float(row['p50_ms'])   if row['p50_ms']   not in ('', '0.0', '0') else None

            hit_rates.append(hit_rate)
            p50s.append(p50)

            if status == 'ok':
                ok_times.append(ts_ms)

    # Blackout = largest gap between consecutive ok rows
    ok_times.sort()
    max_gap_ms, blackout_start_t, blackout_end_t = 0, None, None
    for i in range(1, len(ok_times)):
        gap = ok_times[i] - ok_times[i - 1]
        if gap > max_gap_ms:
            max_gap_ms = gap
            blackout_start_t = (ok_times[i - 1] - event_ms) / 1000.0
            blackout_end_t   = (ok_times[i]     - event_ms) / 1000.0

    return {
        'times':            times,
        'hit_rates':        hit_rates,
        'p50s':             p50s,
        'statuses':         statuses,
        'blackout_start_t': blackout_start_t,
        'blackout_end_t':   blackout_end_t,
        'blackout_s':       max_gap_ms / 1000.0,
    }

# ---------------------------------------------------------------------------
# Plotting helpers
# ---------------------------------------------------------------------------

def _ok_series(data, key, smooth=False, smooth_window=None):
    """Return (times, values) keeping only ok rows with non-None values.
    When smooth=True, apply a causal (look-back only) rolling average so that
    the initial drop at t=0 is not diluted by future recovered values."""
    w = smooth_window if smooth_window is not None else SMOOTH_WINDOW
    pts = [(t, v) for t, s, v in zip(data['times'], data['statuses'], data[key])
           if s == 'ok' and v is not None]
    if not pts:
        return [], []
    times, values = zip(*pts)
    times, values = list(times), list(values)
    if smooth and len(values) >= w:
        smoothed = []
        for i in range(len(values)):
            window = values[max(0, i - w + 1): i + 1]   # causal: only past values
            smoothed.append(sum(window) / len(window))
        values = smoothed
    return times, values


def shade_blackout(ax, data, color, label_y_axes=0.97, label_side='above'):
    """Shade the blackout window and annotate its duration."""
    s, e = data['blackout_start_t'], data['blackout_end_t']
    if s is None:
        return
    # Ensure a minimum visible width (at least 3s at plot scale)
    vis_e = max(e, s + 3)
    ax.axvspan(s, vis_e, color=color, alpha=ALPHA_SHADE, linewidth=0)
    # Bracket lines at real edges
    for x in (s, e):
        ax.axvline(x, color=color, linewidth=0.6, linestyle=':', alpha=0.6)
    dur = data['blackout_s']
    mid = (s + e) / 2
    ax.annotate(
        f'{dur:.0f}s',
        xy=(mid, label_y_axes), xycoords=('data', 'axes fraction'),
        ha='center', va='top', fontsize=8, color=color, fontweight='bold',
        bbox=dict(boxstyle='round,pad=0.15', fc='white', ec=color,
                  linewidth=0.6, alpha=0.85),
    )

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--out', default='benchmark_results.pdf',
                        help='Output file (pdf/png/svg)')
    parser.add_argument('--dir', default=os.path.dirname(os.path.abspath(__file__)),
                        help='Directory containing the CSV/meta files')
    args = parser.parse_args()

    evict   = load_trial(args.dir, 'evict')
    migrate = load_trial(args.dir, 'migrate')

    fig, (ax_hr, ax_p99) = plt.subplots(
        2, 1, figsize=(8, 5.5), sharex=True,
        gridspec_kw={'hspace': 0.08},
    )

    # ---- Top panel: hit rate ------------------------------------------------
    # Draw shading first so lines render on top
    shade_blackout(ax_hr, evict,   COLORS['evict'],   label_y_axes=0.97)
    shade_blackout(ax_hr, migrate, COLORS['migrate'],  label_y_axes=0.85)

    for strategy, data in [('evict', evict), ('migrate', migrate)]:
        color = COLORS[strategy]
        t, hr = _ok_series(data, 'hit_rates', smooth=True)
        ax_hr.plot(t, [v * 100 for v in hr],
                   color=color, linewidth=1.5, alpha=0.9)

    ax_hr.axvline(0, color='black', linewidth=0.8, linestyle='--', alpha=0.5)
    ax_hr.set_ylabel('Cache hit rate (%)', fontsize=10)
    ax_hr.set_ylim(-5, 108)
    ax_hr.set_yticks([0, 25, 50, 75, 100])
    ax_hr.yaxis.set_tick_params(labelsize=9)
    ax_hr.grid(axis='y', linestyle=':', linewidth=0.5, alpha=0.6)
    ax_hr.set_xlim(-305, 305)

    # ---- Bottom panel: p99 latency -----------------------------------------
    shade_blackout(ax_p99, evict,   COLORS['evict'],   label_y_axes=0.97)
    shade_blackout(ax_p99, migrate, COLORS['migrate'],  label_y_axes=0.82)

    for strategy, data in [('evict', evict), ('migrate', migrate)]:
        color = COLORS[strategy]
        t, p50 = _ok_series(data, 'p50s', smooth=True, smooth_window=30)
        ax_p99.plot(t, p50, color=color, linewidth=1.5, alpha=0.9)

    ax_p99.axvline(0, color='black', linewidth=0.8, linestyle='--', alpha=0.5)
    ax_p99.set_ylabel('p50 latency (ms)', fontsize=10)
    ax_p99.set_xlabel('Time relative to event (s)', fontsize=10)
    ax_p99.set_yscale('log')
    ax_p99.set_ylim(1, 100)
    ax_p99.yaxis.set_major_locator(matplotlib.ticker.LogLocator(base=10, numticks=4))
    ax_p99.yaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(
        lambda x, _: f'{int(x)}'))
    ax_p99.yaxis.set_minor_locator(matplotlib.ticker.NullLocator())
    ax_p99.yaxis.set_tick_params(labelsize=9)
    ax_p99.xaxis.set_tick_params(labelsize=9)
    ax_p99.grid(axis='y', linestyle=':', linewidth=0.5, alpha=0.6)

    # ---- Shared legend ------------------------------------------------------
    legend_elements = [
        Line2D([0], [0], color=COLORS['evict'],  linewidth=2, label='Pod eviction'),
        Line2D([0], [0], color=COLORS['migrate'], linewidth=2, label='Pod migration'),
        mpatches.Patch(facecolor=COLORS['evict'],   alpha=0.35, label='Eviction blackout'),
        mpatches.Patch(facecolor=COLORS['migrate'],  alpha=0.35, label='Migration blackout'),
        Line2D([0], [0], color='black', linewidth=0.8, linestyle='--',
               alpha=0.5, label='Event trigger (t = 0)'),
    ]
    ax_hr.legend(handles=legend_elements, loc='lower right',
                 fontsize=8.5, framealpha=0.9, ncol=1)

    # ---- Annotations --------------------------------------------------------
    ax_hr.text(0.01, 0.97, 'a', transform=ax_hr.transAxes,
               fontsize=11, fontweight='bold', va='top')
    ax_p99.text(0.01, 0.97, 'b', transform=ax_p99.transAxes,
                fontsize=11, fontweight='bold', va='top')

    out_path = os.path.join(args.dir, args.out)
    fig.savefig(out_path, bbox_inches='tight', dpi=200)
    print(f'Saved: {out_path}')


if __name__ == '__main__':
    main()
