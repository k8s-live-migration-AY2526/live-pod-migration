#!/usr/bin/env python3
"""
Zipfian Redis GET client for cache-preservation benchmark.

Replaces redis-benchmark for the continuous GET load. Key behaviours:
  - Zipfian key distribution: hot keys are accessed far more often than cold ones
  - Cache-aside on miss: sleep MISS_LATENCY_MS (simulates a DB round-trip),
    then SET the key so it warms into the cache
  - Measures end-to-end per-request latency (includes simulated DB time for misses)
  - Writes one CSV row per second:
      timestamp_ms, delta_hits, delta_misses, hit_rate, p50_ms, p99_ms, throughput, status

Run until killed (SIGTERM / SIGINT); flushes a final partial row on exit.
"""

import argparse, csv, math, os, random, signal, socket, sys, time

# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def _args():
    p = argparse.ArgumentParser()
    p.add_argument('--host', default='127.0.0.1')
    p.add_argument('--port', type=int, default=6379)
    p.add_argument('--num-keys', type=int, default=100_000)
    p.add_argument('--stats-file', required=True)
    p.add_argument('--miss-latency-ms', type=float, default=50.0,
                   help='Simulated DB round-trip added to every cache miss (ms)')
    p.add_argument('--zipf-s', type=float, default=1.0,
                   help='Zipfian exponent s (1.0 = standard Zipf)')
    return p.parse_args()

# ---------------------------------------------------------------------------
# Zipfian sampler — inverse-CDF approximation (O(1) per sample, no deps)
# ---------------------------------------------------------------------------

_GAMMA = 0.5772156649015329  # Euler-Mascheroni constant

def _make_zipf_sampler(n, s=1.0):
    """Return a callable that draws key indices in [0, n-1] from Zipf(s)."""
    if abs(s - 1.0) < 1e-9:
        # For s=1: CDF(k) ≈ (ln k + γ) / (ln N + γ)  →  inverse: k = exp(u·H_N - γ)
        h_n = math.log(n) + _GAMMA
        def _sample():
            while True:
                u = random.random()
                if u == 0:
                    continue
                k = int(math.exp(u * h_n - _GAMMA))
                return max(0, min(n - 1, k))
        return _sample
    else:
        # General exponent: pre-build alias/CDF table (O(N) once, O(log N) per sample)
        import bisect
        weights = [i ** (-s) for i in range(1, n + 1)]
        total = sum(weights)
        cdf, cum = [], 0.0
        for w in weights:
            cum += w / total
            cdf.append(cum)
        def _sample():
            return bisect.bisect_left(cdf, random.random())
        return _sample

# ---------------------------------------------------------------------------
# Minimal RESP client (no third-party deps, auto-reconnects on failure)
# ---------------------------------------------------------------------------

class _Redis:
    def __init__(self, host, port, timeout=1.5):
        self._addr = (host, port)
        self._timeout = timeout
        self._sock = None
        self._buf = b''

    def _connect(self):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(self._timeout)
            s.connect(self._addr)
            s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            self._sock, self._buf = s, b''
            return True
        except OSError:
            self._sock = None
            return False

    def _send(self, *args):
        parts = [f'*{len(args)}\r\n'.encode()]
        for a in args:
            b = a if isinstance(a, bytes) else str(a).encode()
            parts += [f'${len(b)}\r\n'.encode(), b, b'\r\n']
        self._sock.sendall(b''.join(parts))

    def _readline(self):
        while True:
            i = self._buf.find(b'\r\n')
            if i >= 0:
                line, self._buf = self._buf[:i], self._buf[i + 2:]
                return line
            chunk = self._sock.recv(4096)
            if not chunk:
                raise EOFError
            self._buf += chunk

    def _read_resp(self):
        line = self._readline()
        t, rest = chr(line[0]), line[1:]
        if t == '+':
            return rest.decode()
        if t == '-':
            raise RuntimeError(rest.decode())
        if t == ':':
            return int(rest)
        if t == '$':
            n = int(rest)
            if n == -1:
                return None
            while len(self._buf) < n + 2:
                self._buf += self._sock.recv(4096)
            data, self._buf = self._buf[:n], self._buf[n + 2:]
            return data
        if t == '*':
            return [self._read_resp() for _ in range(int(rest))]
        raise ValueError(f'unknown RESP prefix {t!r}')

    def cmd(self, *args):
        """Send command; return (result, err_str). Reconnects transparently."""
        if self._sock is None and not self._connect():
            return None, 'connect_failed'
        try:
            self._send(*args)
            return self._read_resp(), None
        except (OSError, EOFError, RuntimeError, ValueError, socket.timeout) as e:
            self._sock = None
            return None, str(e)

# ---------------------------------------------------------------------------
# Per-second stats window
# ---------------------------------------------------------------------------

class _Window:
    def __init__(self):
        self.hits = self.misses = self.errors = 0
        self._lat = []

    def record_hit(self, ms):
        self.hits += 1
        self._lat.append(ms)

    def record_miss(self, ms):
        self.misses += 1
        self._lat.append(ms)

    def record_error(self):
        self.errors += 1

    def _pct(self, p):
        if not self._lat:
            return 0.0
        s = sorted(self._lat)
        return s[max(0, int(math.ceil(p / 100 * len(s))) - 1)]

    def to_row(self, ts_ms):
        total = self.hits + self.misses
        return {
            'timestamp_ms':  ts_ms,
            'delta_hits':    self.hits,
            'delta_misses':  self.misses,
            'hit_rate':      round(self.hits / total, 4) if total else 'N/A',
            'p50_ms':        round(self._pct(50), 2),
            'p99_ms':        round(self._pct(99), 2),
            'throughput':    total + self.errors,
            'status':        'ok' if total > 0 else 'connection_lost',
        }

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

_FIELDS = ['timestamp_ms', 'delta_hits', 'delta_misses', 'hit_rate',
           'p50_ms', 'p99_ms', 'throughput', 'status']
_MISS_VAL = b'x' * 16

_running = True

def _stop(sig, frame):
    global _running
    _running = False

def main():
    global _running
    a = _args()
    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT,  _stop)

    sample  = _make_zipf_sampler(a.num_keys, a.zipf_s)
    miss_s  = a.miss_latency_ms / 1000.0
    r       = _Redis(a.host, a.port)

    write_header = not os.path.exists(a.stats_file)
    fh = open(a.stats_file, 'a', newline='')
    w  = csv.DictWriter(fh, fieldnames=_FIELDS)
    if write_header:
        w.writeheader()
        fh.flush()

    win        = _Window()
    next_tick  = time.monotonic() + 1.0

    while _running:
        key = f'key:{sample():012d}'.encode()

        t0          = time.monotonic()
        val, err    = r.cmd('GET', key)

        if err:
            win.record_error()
        elif val is not None:
            win.record_hit((time.monotonic() - t0) * 1000)
        else:
            time.sleep(miss_s)          # simulate DB round-trip
            r.cmd('SET', key, _MISS_VAL)  # populate cache (cache-aside)
            win.record_miss((time.monotonic() - t0) * 1000)

        now = time.monotonic()
        if now >= next_tick:
            row = win.to_row(int(time.time() * 1000))
            w.writerow(row)
            fh.flush()
            win       = _Window()
            next_tick = now + 1.0

    # flush final partial window
    if win.hits + win.misses + win.errors:
        w.writerow(win.to_row(int(time.time() * 1000)))
        fh.flush()
    fh.close()

if __name__ == '__main__':
    main()
