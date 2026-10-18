#!/usr/bin/env python3
"""Lens as the lens_app role on compose (B39.6, issue #186).

    python3 scripts/app-role/run.py [--build] [--keep]

Brings up the production compose with LENS_APP_DB_PASSWORD set (project lens-approle, the db-kill
overlay's mock upstream), then passes when:
  - every connection PgBouncer holds for Lens is lens_app, none is lens;
  - a funded workspace's request is served 200 and its charge lands in lxc_ledger as a spend row;
  - logged in as lens_app, UPDATE money_postings and ALTER TABLE money_postings DISABLE TRIGGER are
    both refused with SQLSTATE 42501 and the trigger is still enabled.
Without --keep the stack and its volumes are removed at the end.
"""
import json
import os
import pathlib
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
BASE = 'http://127.0.0.1:8080'
ADMIN = secrets.token_hex(32)
APP_PW = secrets.token_hex(24)
WS = 'approle-1'
ENV = dict(os.environ, POSTGRES_PASSWORD='approle', LENS_APP_DB_PASSWORD=APP_PW, LENS_API_KEY=ADMIN,
           LENS_DOMAIN='localhost', MEMBER_SYNC_SECRET='unused', DOCS_GATEWAY_AUTH_SECRET='unused',
           TRACK_GATEWAY_AUTH_SECRET='unused')
COMPOSE = ['docker', 'compose', '-p', 'lens-approle', '-f', str(ROOT / 'docker-compose.yaml'),
           '-f', str(ROOT / 'scripts/db-kill/compose.yaml')]


def compose(*args, check=True, capture=False):
    print('$ docker compose', *args, flush=True)
    return subprocess.run(COMPOSE + list(args), env=ENV, cwd=ROOT, check=check, text=True,
                          capture_output=capture)


def psql(sql, user='lens', password='approle'):
    # Over TCP, so lens_app logs in with its password rather than the socket's trust.
    r = compose('exec', '-T', '-e', 'PGPASSWORD=' + password, 'postgres', 'psql', '-h', '127.0.0.1',
                '-U', user, '-d', 'talyvor_lens', '-v', 'VERBOSITY=verbose', '-tAc', sql,
                check=False, capture=True)
    return r.returncode, (r.stdout + r.stderr).strip()


def http(method, path, key, body=None):
    req = urllib.request.Request(BASE + path, method=method, data=json.dumps(body).encode() if body else None,
                                 headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def check(name, ok, detail):
    print(('PASS ' if ok else 'FAIL ') + name + ': ' + detail, flush=True)
    return ok


def main():
    compose('up', '-d', '--wait', *(['--build'] if '--build' in sys.argv else []), 'lens', 'mockvllm')
    try:
        ok = []
        st, body = http('POST', '/v1/api/keys', ADMIN, {'workspace_id': WS, 'name': 'approle'})
        assert st in (200, 201), (st, body)
        key = json.loads(body)['key']
        st, body = http('POST', '/v1/admin/lxc/grant', ADMIN, {'workspace_id': WS, 'amount_ulxc': 10**12})
        assert st == 200, ('grant failed', st, body)

        st, body = http('POST', '/v1/proxy/vllm/v1/chat/completions', key,
                        {'model': 'trial-mock', 'messages': [{'role': 'user', 'content': 'b39.6 ' + APP_PW}]})
        ok.append(check('request served', st == 200, f'{st} {body[:120]!r}'))
        debit = ''
        for _ in range(20):  # the settled charge (type spend; the hold is only a bound) lands after the answer
            _, debit = psql(f"SELECT type || ' ' || amount FROM lxc_ledger WHERE workspace_id = '{WS}' "
                            "AND type = 'spend' AND amount < 0")
            if debit:
                break
            time.sleep(0.5)
        ok.append(check('request billed', bool(debit), 'lxc_ledger spend row: ' + (debit or 'none')))

        _, users = psql("SELECT string_agg(DISTINCT usename, ',') FROM pg_stat_activity "
                        "WHERE datname = 'talyvor_lens' AND pid <> pg_backend_pid() AND backend_type = 'client backend'")
        ok.append(check('Lens logs in as lens_app', users == 'lens_app', 'connections: ' + users))

        for sql in ('UPDATE money_postings SET amount_minor = 1',
                    'ALTER TABLE money_postings DISABLE TRIGGER audit_no_mutation'):
            rc, out = psql(sql, user='lens_app', password=APP_PW)
            ok.append(check('as lens_app: ' + sql, rc != 0 and '42501' in out, out.splitlines()[0] if out else ''))
        _, enabled = psql("SELECT tgenabled FROM pg_trigger WHERE tgrelid = 'money_postings'::regclass "
                          "AND tgname = 'audit_no_mutation'")
        ok.append(check('trigger still enabled', enabled == 'O', 'tgenabled=' + enabled))

        print('\nPASS' if all(ok) else '\nFAIL')
        if not all(ok):
            compose('logs', '--no-color', '--tail', '200', 'migrate', 'lens', 'pgbouncer', check=False)
            sys.exit(1)
    finally:
        if '--keep' not in sys.argv:
            compose('down', '-v', check=False)


if __name__ == '__main__':
    main()
