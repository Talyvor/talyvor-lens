#!/usr/bin/env python3
"""Postgres kill test (B39.5, issue #174): take Postgres away under load, bring it back, measure.

    python3 scripts/db-kill/run.py [--build] [--keep]

Brings up the production compose (lens, pgbouncer, postgres, redis, nats, migrate) plus a mock
upstream under the project name lens-dbkill, funds one agent key per client, and runs WORKERS
concurrent clients sending distinct prompts (no cache hits). Two outages, each after WARM seconds of
load, lasting DOWN seconds, then AFTER seconds of load:

  kill   SIGKILL the postgres container, then start it again;
  pause  freeze it (docker pause), then unpause it: a query caught mid-flight gets no answer and no
         reset, which is how #174's 25 requests sat until the 60 s request timeout.

Each passes when no request waits more than 10 s, every request that failed answered 503, and
/healthz answers 200 again within 20 s of postgres being back. Per-request results and Lens's
/healthz three seconds into each outage (its in-flight requests and the phase each is in) go to
scripts/db-kill/out/. Without --keep the stack and its volumes are removed at the end.
"""
import json
import os
import pathlib
import secrets
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
OUT = ROOT / 'scripts/db-kill/out'
BASE = 'http://127.0.0.1:8080'
WORKERS, WARM, DOWN, AFTER = 25, 10, 30, 30
MAX_WAIT, MAX_RECOVERY = 10.0, 20.0
PACE = 0.1  # 10 a second per client: 600 a minute, under each workspace's 1,000
ADMIN = secrets.token_hex(32)
ENV = dict(os.environ, POSTGRES_PASSWORD='dbkill', LENS_API_KEY=ADMIN, LENS_DOMAIN='localhost',
           MEMBER_SYNC_SECRET='unused', DOCS_GATEWAY_AUTH_SECRET='unused', TRACK_GATEWAY_AUTH_SECRET='unused')
COMPOSE = ['docker', 'compose', '-p', 'lens-dbkill', '-f', str(ROOT / 'docker-compose.yaml'),
           '-f', str(ROOT / 'scripts/db-kill/compose.yaml')]


def compose(*args, check=True):
    print('$ docker compose', *args, flush=True)
    return subprocess.run(COMPOSE + list(args), env=ENV, cwd=ROOT, check=check)


def http(method, path, key, body=None, timeout=90):
    req = urllib.request.Request(BASE + path, method=method, data=json.dumps(body).encode() if body else None,
                                 headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # timeout / reset: recorded as status 0
        return 0, str(e).encode()


def main():
    OUT.mkdir(exist_ok=True)
    compose('up', '-d', '--wait', *(['--build'] if '--build' in sys.argv else []),
            'lens', 'mockvllm')
    try:
        keys = setup()
        ok = [outage(keys, mode) for mode in ('kill', 'pause')]
        print('\nPASS' if all(ok) else '\nFAIL')
        if not all(ok):
            compose('logs', '--no-color', '--tail', '400', 'lens', 'pgbouncer', check=False)
            sys.exit(1)
    finally:
        if '--keep' not in sys.argv:
            compose('down', '-v', check=False)


def setup():
    # One workspace per client: a single workspace's limiter (1,000 a minute)
    # would refuse most of the load with 429 before it reached the database.
    keys = []
    for i in range(WORKERS):
        ws = f'dbkill-{i}'
        st, body = http('POST', '/v1/api/keys', ADMIN, {'workspace_id': ws, 'name': 'dbkill'})
        assert st in (200, 201), (st, body)
        keys.append(json.loads(body)['key'])
        st, body = http('POST', '/v1/admin/lxc/grant', ADMIN, {'workspace_id': ws, 'amount_ulxc': 10**12})
        assert st == 200, ('grant failed', st, body)
    st, body = http('POST', '/v1/proxy/vllm/v1/chat/completions', keys[0],
                    {'model': 'trial-mock', 'messages': [{'role': 'user', 'content': 'warm-up'}]})
    assert st == 200, ('warm-up request failed', st, body[:300])
    return keys


def outage(keys, mode):
    results, stop = [], threading.Event()

    def worker(i):
        n = 0
        while not stop.is_set():
            n += 1
            t0 = time.time()
            st, body = http('POST', '/v1/proxy/vllm/v1/chat/completions', keys[i],
                            {'model': 'trial-mock', 'messages': [{'role': 'user', 'content': f'dbkill {mode} {i} {n} {t0}'}]})
            results.append((t0, time.time(), st, body[:200]))
            time.sleep(max(0, PACE - (time.time() - t0)))

    print(f'\n== {mode}', flush=True)
    threads = [threading.Thread(target=worker, args=(i,), daemon=True) for i in range(WORKERS)]
    for t in threads:
        t.start()
    time.sleep(WARM)
    t_kill = time.time()
    compose(*(['kill', '-s', 'SIGKILL'] if mode == 'kill' else ['pause']), 'postgres')
    time.sleep(3)
    (OUT / f'healthz-{mode}.json').write_bytes(http('GET', '/healthz', ADMIN)[1])
    time.sleep(DOWN - 3)
    compose('start' if mode == 'kill' else 'unpause', 'postgres')
    t_back = time.time()
    t_healthy = None
    while time.time() - t_back < AFTER:
        if t_healthy is None and http('GET', '/healthz', ADMIN, timeout=5)[0] == 200:
            t_healthy = time.time()
        time.sleep(0.5)
    stop.set()
    for t in threads:
        t.join(100)

    (OUT / f'results-{mode}.json').write_text(json.dumps(
        {'kill': t_kill, 'back': t_back, 'healthy': t_healthy,
         'requests': [(a, b, c, d.decode(errors='replace')) for a, b, c, d in results]}))
    slow = [r for r in results if r[1] - r[0] > MAX_WAIT]
    failed = [r for r in results if r[2] != 200]
    not503 = [r for r in failed if r[2] != 503]
    caught = [r for r in results if r[0] < t_back and r[1] > t_kill]
    by_status = {}
    for r in failed:
        by_status[r[2]] = by_status.get(r[2], 0) + 1
    print(f'{len(results)} requests; {len(caught)} overlapped the outage; failed by status: {by_status}')
    print(f'longest wait: {max(r[1] - r[0] for r in results):.1f}s; over {MAX_WAIT:.0f}s: {len(slow)}')
    for r in (not503 + slow)[:10]:
        print(f'  t+{r[0] - t_kill:.1f}s waited {r[1] - r[0]:.1f}s -> {r[2]} {r[3]!r}')
    rec = None if t_healthy is None else t_healthy - t_back
    print('healthz 200 again', 'never' if rec is None else f'{rec:.1f}s after postgres was back')
    ok = not slow and not not503 and rec is not None and rec <= MAX_RECOVERY
    print(mode, 'PASS' if ok else 'FAIL', flush=True)
    return ok


if __name__ == '__main__':
    main()
