"""Functional tests. All HTTP connections go to ephemeral loopback fixtures."""
import hashlib
import json
import os
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

import application_evidence as app
import runner


SUBJECT = {'asset_id': 'api', 'cluster': 'east', 'instance': 'a'}
CANARY = 'synthetic-private-response-marker'


@contextmanager
def serve(tls=None, on_post=None):
    routes, visits = {}, []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            raw = self.rfile.read(int(self.headers['Content-Length']))
            reply = on_post(json.loads(raw))
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps(reply).encode())

        def do_GET(self):
            visits.append((self.path, self.headers))
            status, headers, data = routes[self.path]
            if status is None:
                self.connection.shutdown(socket.SHUT_RDWR)
                return
            if callable(data):
                data = data()
            raw = data if isinstance(data, bytes) else json.dumps(data).encode()
            self.send_response(status)
            for key, value in headers.items():
                self.send_header(key, value)
            self.end_headers()
            try:
                self.wfile.write(raw)
            except BrokenPipeError:
                pass

        def log_message(self, *args):
            pass

    with ThreadingHTTPServer(('127.0.0.1', 0), Handler) as server:
        if tls:
            server.socket = tls.wrap_socket(server.socket, server_side=True)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield f'{"https" if tls else "http"}://127.0.0.1:{server.server_port}', routes, visits
        finally:
            server.shutdown()
            thread.join()


class ApplicationEvidenceTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.policy = self.root / 'policy.json'
        self.folder = self.root / 'east' / 'a' / 'api'
        self.http = serve()
        self.base, self.routes, self.visits = self.http.__enter__()
        self.addCleanup(self.http.__exit__, None, None, None)
        self.json_headers = {'Content-Type': 'application/json'}

    def config(self, checks):
        self.policy.write_text(json.dumps(SUBJECT | {'checks': checks}))
        self.policy.chmod(0o600)

    def read(self, check):
        return json.loads((self.folder / (check + '.json')).read_text())

    def scalar(self, metric, value, unit):
        return SUBJECT | {'metric': metric, 'unit': unit, 'value': value, 'observed_at': datetime.now(timezone.utc).isoformat()}

    def test_cli_all_checks_and_runner_receipt(self):
        status = {'url': self.base + '/health', 'status': 200}
        self.config({'A1': status, 'A2': {'url': self.base + '/db'}, 'A3': {'url': self.base + '/traffic', 'min': 2, 'max': 12},
                     'A4': {'url': self.base + '/web'}, 'A5': {'url': self.base + '/api', 'field': ['data', 'ready'], 'equals': True, 'status': 200}})
        self.routes.update({'/health': (200, {'Set-Cookie': CANARY}, CANARY), '/db': (200, self.json_headers, self.scalar('database_up', 0, '1')),
                            '/traffic': (200, self.json_headers, self.scalar('requests_per_second', 12, 'req/s')), '/web': (200, {}, b''),
                            '/api': (200, self.json_headers, {'data': {'ready': True}, 'private': CANARY})})
        env = dict(os.environ, HTTP_PROXY='http://127.0.0.1:1', NO_PROXY='')
        run = subprocess.run([sys.executable, '-B', app.__file__, '--config', str(self.policy), '--root', str(self.root)],
                             capture_output=True, text=True, timeout=10, env=env, check=True)
        self.assertEqual(json.loads(run.stdout), {'published': 5, 'findings': 2, 'gaps': 0})
        self.assertNotIn(CANARY, run.stdout + run.stderr)
        self.assertEqual([path for path, _ in self.visits], ['/health', '/db', '/traffic', '/web', '/api'])
        for _, h in self.visits:
            self.assertFalse(any(k in h for k in ('Cookie', 'Authorization', 'Proxy-Authorization')))
        for check, counts in {'A1': (1, 0), 'A2': (1, 1), 'A3': (1, 0), 'A4': (3, 3), 'A5': (1, 0)}.items():
            with self.subTest(check=check):
                record = runner.receipt(self.folder, SUBJECT | {'check_id': check})
                self.assertEqual((record['samples'], record['violations']), counts)
                raw = (self.folder / f'{check}.json').read_bytes()
                self.assertNotIn(CANARY.encode(), raw)
                self.assertEqual(record['sha256'], hashlib.sha256(raw).hexdigest())
        self.assertEqual(self.read('A3')['observed_at'], self.routes['/traffic'][2]['observed_at'])

    def test_missing_and_denied_replace_previous_pass(self):
        self.config({'A1': {'url': self.base + '/health', 'status': 200}})
        for status, outcome in [(200, 0), (503, 1), (401, 'permission_denied'), (403, 'permission_denied'), (302, 'redirect_not_followed'), (429, 'rate_limited')]:
            self.routes['/health'] = (status, {'Location': self.base + '/forbidden', 'Set-Cookie': CANARY}, CANARY)
            app.run(self.policy, self.root)
            result = self.read('A1')
            if isinstance(outcome, int):
                self.assertEqual(result['violations'], outcome)
            else:
                self.assertEqual(result['gap'], outcome)
                self.assertEqual(runner.receipt(self.folder, SUBJECT | {'check_id': 'A1'})['gap'], outcome)
                self.assertNotIn('samples', result)
                self.assertNotIn('violations', result)
            self.assertNotIn(CANARY, json.dumps(result))
        self.assertEqual(set(p for p, _ in self.visits), {'/health'})
        self.routes['/health'] = (None, {}, b'')  # Peer disconnects before sending any HTTP response.
        app.run(self.policy, self.root)
        self.assertEqual(self.read('A1')['gap'], 'source_unreachable')

    def test_exporter_identity_freshness_and_api_field(self):
        db = self.scalar('database_up', 1, '1')
        rule = {'url': self.base + '/db'}
        for change, outcome in [({'value': 1}, 0), ({'value': False}, 'invalid_exporter_scalar'), ({'metric': 'other'}, 'exporter_subject_or_metric_mismatch'),
                                ({'unit': 'ms'}, 'exporter_subject_or_metric_mismatch'), ({'asset_id': 'other'}, 'exporter_subject_or_metric_mismatch'),
                                ({'observed_at': (datetime.now(timezone.utc) - timedelta(hours=1)).isoformat()}, 'stale_or_future_exporter_sample')]:
            self.routes['/db'] = (200, self.json_headers, db | change)
            result = app.collect('A2', rule, SUBJECT)
            self.assertEqual(result.get('gap', result.get('violations')), outcome)
        self.routes['/db'] = (200, self.json_headers, self.scalar('requests_per_second', 0, 'req/s'))
        self.assertEqual(app.collect('A3', rule | {'min': 1, 'max': 2}, SUBJECT)['violations'], 1)
        policy = rule | {'field': ['active'], 'equals': True, 'status': 200}
        for body, outcome in [({'active': True}, 0), ({'active': 1}, 1), ({'private': CANARY}, 1),
                              (b'{"active":true,"active":false}', 'invalid_response'), (b'x' * (app.BODY_LIMIT + 1), 'response_too_large')]:
            self.routes['/db'] = (200, self.json_headers, body)
            result = app.collect('A5', policy, SUBJECT)
            self.assertEqual(result.get('gap', result.get('violations')), outcome)
            self.assertNotIn(CANARY, json.dumps(result))

    def test_policy_limits_and_deadline(self):
        bad_urls = ['file:///etc/hosts', self.base + '?key=private', self.base.replace('://', '://user@'), self.base + '/a/../b']
        for url in bad_urls:
            self.assertEqual(app.collect('A1', {'status': 200, 'url': url}, SUBJECT)['gap'], 'invalid_probe_policy')
        self.assertEqual(app.collect('A1', {'url': self.base, 'status': 200, 'method': 'POST'}, SUBJECT)['gap'], 'invalid_probe_policy')
        self.assertFalse(self.visits)
        self.config({'A1': {'url': self.base + '/slow', 'status': 200}})
        self.policy.chmod(0o666)
        with self.assertRaises(ValueError):
            app.run(self.policy, self.root)
        self.assertFalse(self.visits)
        self.policy.chmod(0o600)
        self.routes['/slow'] = (200, {}, lambda: (time.sleep(.25), b'')[1])
        with patch.object(app, 'TIMEOUT', .08):
            app.run(self.policy, self.root)
        self.assertEqual(self.read('A1')['gap'], 'probe_timeout')

    def test_publication_is_atomic(self):
        self.config({'A1': {'url': self.base + '/s', 'status': 200}})
        self.routes['/s'] = (200, {}, b'')
        app.run(self.policy, self.root)
        previous = (self.folder / 'A1.json').read_bytes()
        self.routes['/s'] = (503, {}, b'')
        with patch.object(os, 'replace', side_effect=OSError('fixture publication failure')), self.assertRaises(OSError):
            app.run(self.policy, self.root)
        self.assertEqual((self.folder / 'A1.json').read_bytes(), previous)
        self.assertEqual([p.name for p in self.folder.iterdir()], ['A1.json'])

    def test_runner_submits_measurements_and_gaps(self):
        self.config({'A1': {'url': self.base + '/health', 'status': 200}, 'A2': {'url': self.base + '/db'}})
        self.routes.update({'/health': (503, {}, b''), '/db': (401, {}, CANARY)})
        output = subprocess.run([sys.executable, '-B', app.__file__, '--config', str(self.policy), '--root', str(self.root)],
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(output.returncode, 2)
        self.assertEqual(json.loads(output.stdout), {'published': 2, 'findings': 1, 'gaps': 1})
        received = {}

        def accept(message):
            if message['action'] == 'tick':
                return {}
            received[message['item_id']] = message['evidence']
            return {'status': 'insufficient' if 'gap' in message['evidence'] else 'finding'}

        with serve(on_post=accept) as (base, routes, _):
            work = [SUBJECT | {'check_id': c, 'item_id': i, 'generation': 1, 'opened': int(time.time()) - 30}
                    for i, c in enumerate(('A1', 'A2'), 1)]
            routes['/v1/assets/inspections?view=work'] = (200, self.json_headers, {'work': work})
            token = self.root / 'fixture-token'
            token.write_text('local-fixture-only')
            env = dict(os.environ, INSPECTION_GATEWAY=base, INSPECTION_TOKEN_FILE=str(token),
                       INSPECTION_EVIDENCE_ROOT=str(self.root), NO_PROXY='*')
            run = subprocess.run([sys.executable, '-B', runner.__file__], capture_output=True, text=True,
                                 timeout=10, env=env, check=True)
        self.assertEqual(json.loads(run.stdout)['gaps'], 1)
        self.assertEqual(received[1]['violations'], 1)
        self.assertTrue(received[2]['gap'])
        self.assertNotIn('samples', received[2])
        self.assertNotIn(CANARY, str(received))

    def test_tls_verified_and_failed(self):
        cert, key = self.root / 'fixture.crt', self.root / 'fixture.key'
        # Generate a short-lived loopback-only fixture. Neither key nor body enters a receipt.
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=localhost',
                        '-addext', 'subjectAltName=IP:127.0.0.1', '-addext', 'basicConstraints=critical,CA:TRUE',
                        '-addext', 'keyUsage=critical,keyCertSign,digitalSignature,keyEncipherment',
                        '-keyout', str(key), '-out', str(cert)], check=True, capture_output=True, timeout=10)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(cert, key)
        with serve(ctx) as (base, routes, visits):
            rule = {'url': base + '/web'}
            headers = {'Strict-Transport-Security': 'max-age=3600; includeSubDomains', 'X-Content-Type-Options': 'nosniff'}
            routes['/web'] = (200, headers, CANARY)
            self.assertEqual(app.collect('A4', rule, SUBJECT)['violations'], 1)
            self.assertFalse(visits)
            with patch.dict(os.environ, {'SSL_CERT_FILE': str(cert)}):
                self.assertEqual(app.collect('A4', rule, SUBJECT)['violations'], 0)
                headers['Strict-Transport-Security'] = 'max-age="3600'
                self.assertEqual(app.collect('A4', rule, SUBJECT)['violations'], 1)


if __name__ == '__main__':
    unittest.main()
