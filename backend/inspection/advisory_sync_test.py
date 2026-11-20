"""Functional checks using a loopback TLS mirror and the public command-line entry points."""
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import ssl
import subprocess
import sys
import tempfile
import threading
import unittest
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import runner

HERE = Path(__file__).resolve().parent


@contextmanager
def serve(handler, context=None):
    with ThreadingHTTPServer(('127.0.0.1', 0), handler) as server:
        if context:
            server.socket = context.wrap_socket(server.socket, server_side=True)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield server
        finally:
            server.shutdown()
            thread.join()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, data, status=200):
        self.send_response(status)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class SignedMirrorTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        self.dest = root / 'state' / 'advisories.json'
        self.dest.parent.mkdir()
        self.key, self.pub = root / 'signer.pem', root / 'review.pub'
        self.ca, tls_key = root / 'cert.pem', root / 'tls.pem'
        def openssl(*args):
            subprocess.run(['openssl', *args], capture_output=True, check=True)
        openssl('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=localhost',
                '-addext', 'subjectAltName=IP:127.0.0.1', '-keyout', tls_key, '-out', self.ca)
        openssl('genpkey', '-algorithm', 'RSA', '-pkeyopt', 'rsa_keygen_bits:2048', '-out', self.key)
        openssl('pkey', '-in', self.key, '-pubout', '-out', self.pub)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(self.ca, tls_key)
        self.payloads = {}
        payloads = self.payloads
        class Mirror(Handler):
            def do_GET(self):
                if self.path == '/redirect':
                    self.send_response(302)
                    self.send_header('Location', '/feed')
                    self.end_headers()
                elif self.path in payloads:
                    self.reply(payloads[self.path])
                else:
                    self.reply(b'', 404)
        self.mirror = serve(Mirror, tls)
        self.url = f'https://127.0.0.1:{self.mirror.__enter__().server_port}/feed'
        self.addCleanup(self.mirror.__exit__, None, None, None)
        self.now = datetime.now(timezone.utc)
        self.rule = {'component': 'pkg:deb/debian/dpkg?arch=amd64&distro=debian-12', 'advisory': 'TEST-ONLY',
                     'kind': 'vulnerability', 'introduced': '0', 'fixed': '1.21.24'}
        self.env = dict(os.environ, NO_PROXY='*', no_proxy='*', INSPECTION_ADVISORY_URL=self.url,
                        INSPECTION_ADVISORY_PUBLIC_KEY=str(self.pub), INSPECTION_ADVISORY_CA=str(self.ca),
                        INSPECTION_EVIDENCE_ROOT=str(self.dest.parent), INSPECTION_ADVISORY_FILE=str(self.dest))

    def feed(self, start=-60, end=600):
        return json.dumps({'issued_at': (self.now + timedelta(seconds=start)).isoformat(),
                           'valid_until': (self.now + timedelta(seconds=end)).isoformat(),
                           'advisories': [self.rule]}).encode()

    def mirror_feed(self, raw):
        self.payloads['/feed'] = raw
        self.payloads['/feed.sig'] = subprocess.check_output(['openssl', 'dgst', '-sha256', '-sign', self.key], input=raw)

    def sync(self, *args, ok=True):
        result = subprocess.run([sys.executable, '-B', HERE / 'advisory_sync.py', *args],
                                env=self.env, capture_output=True, timeout=20)
        self.assertEqual(result.returncode == 0, ok, result.stderr)
        return result

    def test_signed_updates_and_atomic_refusals(self):
        raw = self.feed()
        self.mirror_feed(raw)
        self.assertEqual(json.loads(self.sync().stdout)['sha256'], hashlib.sha256(raw).hexdigest())
        self.sync()
        self.assertEqual(self.dest.stat().st_mode & 0o777, 0o640)
        previous = self.dest.stat().st_ino
        for candidate in (self.feed(-61), self.feed(-60, 70), self.feed(-10, -1), self.feed(3600, 7200),
                          self.feed(-10).replace(b'"fixed":', b'"fixed":"0","fixed":'), b'{}'):
            with self.subTest(candidate=candidate):
                self.mirror_feed(candidate)
                self.sync(ok=False)
                self.assertEqual(self.dest.read_bytes(), raw)
                self.assertEqual(self.dest.stat().st_ino, previous)
        newer = self.feed(-10)
        self.mirror_feed(newer)
        self.payloads['/feed'] = newer + b' '
        self.sync(ok=False)
        self.assertEqual(self.dest.read_bytes(), raw)
        self.mirror_feed(newer)
        for url in (self.url.replace('https:', 'http:'), self.url.replace('/feed', '/redirect')):
            self.sync('--url', url, ok=False)
        self.sync('--public-key', self.ca, ok=False)
        self.sync('--ca-file', self.pub, ok=False)
        self.assertEqual(self.dest.stat().st_ino, previous)
        with self.dest.open('rb') as reader:
            self.sync()
            self.assertEqual(reader.read(), raw)
            self.assertEqual(self.dest.read_bytes(), newer)
        self.assertEqual(sorted(p.name for p in self.dest.parent.iterdir()), ['.advisory-sync.lock', 'advisories.json'])

    def test_runner_reads_only_published_file(self):
        raw = self.feed()
        self.mirror_feed(raw)
        self.sync()
        root = self.dest.parent.parent / 'evidence'
        subject = {'cluster': 'local', 'instance': 'debian12', 'asset_id': 'container'}
        folder = root.joinpath(*subject.values())
        folder.mkdir(parents=True)
        bom = {'bomFormat': 'CycloneDX', 'metadata': {'timestamp': runner.timestamp(), 'properties': [
            {'name': f'doops:{k}', 'value': v} for k, v in subject.items()]}, 'components': [
            {'purl': self.rule['component'].replace('?', '@1.21.23?'), 'version': '1.21.23'}]}
        (folder / 'sbom.json').write_text(json.dumps(bom))
        (root / 'advisories.json').write_bytes(raw)
        token = root / 'token'
        token.write_text('loopback-test')
        submissions = []
        class Gateway(Handler):
            def do_GET(self):
                self.reply(json.dumps({'work': [dict(subject, check_id='V1', item_id=1, generation=1, opened=0)]}).encode())

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                if body['action'] == 'evidence':
                    submissions.append(body['evidence'])
                self.reply(b'{"status":"ok"}')
        with serve(Gateway) as gw:
            env = dict(self.env, INSPECTION_EVIDENCE_ROOT=str(root), INSPECTION_TOKEN_FILE=str(token),
                       INSPECTION_GATEWAY=f'http://127.0.0.1:{gw.server_port}')
            def run():
                subprocess.run([sys.executable, '-B', HERE / 'runner.py'], env=env, capture_output=True, check=True)
                return submissions.pop()
            evidence = run()
            self.assertEqual(evidence['packages'][0]['version'], '1.21.23')
            self.assertEqual(evidence['bulletin_sha256'], hashlib.sha256(raw).hexdigest())
            self.dest.write_bytes(self.feed(-120, -60))
            self.assertEqual(run()['gap'], 'incomplete_or_stale_source')
            self.dest.unlink()
            self.assertEqual(run()['gap'], 'source_missing')


@unittest.skipUnless(sys.platform == 'linux' and shutil.which('dpkg-query'), 'requires a local dpkg database')
class InstalledSBOMTest(unittest.TestCase):
    def test_local_inventory_matches_runner_with_qualified_purl(self):
        distro = platform.freedesktop_os_release()['ID']
        if distro not in ('debian', 'ubuntu'):
            self.skipTest('collector supports Debian and Ubuntu')
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            subject = {'cluster': 'lab', 'instance': 'local', 'asset_id': 'container'}
            result = subprocess.check_output([sys.executable, '-B', HERE / 'runner.py', 'dpkg-sbom', '--root', root,
                                              '--cluster', 'lab', '--instance', 'local', '--asset-id', 'container'])
            receipt = json.loads(result)
            path = Path(receipt['path'])
            raw = path.read_bytes()
            self.assertEqual(hashlib.sha256(raw).hexdigest(), receipt['sha256'])
            bom = json.loads(raw)
            rows = subprocess.check_output(['dpkg-query', '-W', r'-f=${db:Status-Status}\t${Package}\t${Version}\n'], text=True)
            installed = sorted((name, v) for state, name, v in (r.split('\t') for r in rows.splitlines()) if state == 'installed')
            self.assertEqual(sorted((p['name'], p['version']) for p in bom['components']), installed)
            self.assertEqual(receipt['components'], len(installed))
            version, arch = subprocess.check_output(['dpkg-query', '-W', '-f=${Version} ${Architecture}', 'dpkg'], text=True).split()
            release = platform.freedesktop_os_release()['VERSION_ID']
            pkg = f'pkg:deb/{distro}/dpkg?arch={arch}&distro={distro}-{release}'
            feed = root / 'reviewed.json'
            feed.write_text(json.dumps({'issued_at': '2020-01-01T00:00:00Z', 'valid_until': '2099-01-01T00:00:00Z',
                                        'advisories': [{'kind': 'patch', 'advisory': 'TEST-ONLY', 'component': pkg,
                                                        'introduced': '0', 'fixed': '999'}]}))
            matches = runner.software(path.parent, feed, dict(subject, check_id='V2'))['packages']
            self.assertEqual([(p['component'], p['version']) for p in matches], [(pkg, version)])


if __name__ == '__main__':
    unittest.main()
