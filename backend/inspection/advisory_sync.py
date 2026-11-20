"""Install one current, signed advisory feed from an operator-approved HTTPS mirror."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import ssl
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit
from urllib.request import build_opener, HTTPSHandler, Request

from runner import DEFAULT_ADVISORY_FILE, MAX_ARTIFACT, NoRedirect, bounded, epoch


def read_feed(raw, now=None):
    def unique(pairs):
        result = dict(pairs)
        if len(result) != len(pairs):
            raise ValueError('duplicate field')
        return result

    doc = json.loads(raw, object_pairs_hook=unique)
    issued, end = epoch(doc['issued_at']), epoch(doc['valid_until'])
    if end <= issued or (now is not None and not issued <= now < end):
        raise ValueError('feed is outside its validity window')
    rules = doc['advisories']
    if not isinstance(rules, list):
        raise ValueError('expected advisory list')
    seen = set()
    for rule in rules:
        kind, component, advisory = rule['kind'], rule['component'], rule['advisory']
        for value, limit in ((component, 256), (advisory, 128), (rule['introduced'], 128), (rule['fixed'], 128)):
            if not isinstance(value, str) or not 0 < len(value) <= limit or not value.isprintable() or value.strip() != value:
                raise ValueError('invalid advisory field')
        if kind not in ('vulnerability', 'patch') or not component.startswith('pkg:'):
            raise ValueError('invalid component or kind')
        pair = (component, advisory)
        if pair in seen:
            raise ValueError('duplicate advisory for component')
        seen.add(pair)
    return issued, len(rules)


def sync(url, public_key, root, ca_file=None):
    parsed = urlsplit(url)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment:
        raise ValueError('feed must be a configured HTTPS URL without credentials or query')
    opener = build_opener(NoRedirect(), HTTPSHandler(context=ssl.create_default_context(cafile=ca_file)))
    def fetch(address, limit):
        with opener.open(Request(address, headers={'Accept-Encoding': 'identity'}), timeout=12) as response:
            if response.status != 200:
                raise ValueError('unexpected mirror status')
            return bounded(response, limit)

    raw = fetch(url, MAX_ARTIFACT)
    signature = fetch(url + '.sig', 16384)
    dest = root / DEFAULT_ADVISORY_FILE.name
    with tempfile.TemporaryDirectory(dir=root) as tmp:
        candidate = Path(tmp) / 'feed'
        candidate.write_bytes(raw)
        sig = Path(tmp) / 'signature'
        sig.write_bytes(signature)
        subprocess.run(['openssl', 'dgst', '-sha256', '-verify', str(public_key), '-signature', str(sig), str(candidate)],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5, check=True)
        with (root / '.advisory-sync.lock').open('a+b') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            issued, count = read_feed(raw, time.time())
            if dest.exists():
                with dest.open('rb') as current:
                    old = bounded(current, MAX_ARTIFACT)
                prev, _ = read_feed(old)
                if issued < prev or issued == prev and old != raw:
                    raise ValueError('older or conflicting advisory issue')
            with candidate.open('rb') as stream:
                os.fchmod(stream.fileno(), 0o640)
                os.fsync(stream.fileno())
            os.replace(candidate, dest)
            directory = os.open(root, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
    return {'advisories': count, 'sha256': hashlib.sha256(raw).hexdigest(), 'issued_at_unix': issued}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', default=os.environ.get('INSPECTION_ADVISORY_URL'), required=not os.environ.get('INSPECTION_ADVISORY_URL'))
    parser.add_argument('--public-key', type=Path, default=os.environ.get('INSPECTION_ADVISORY_PUBLIC_KEY'), required=not os.environ.get('INSPECTION_ADVISORY_PUBLIC_KEY'))
    parser.add_argument('--root', type=Path, default=os.environ.get('INSPECTION_EVIDENCE_ROOT', DEFAULT_ADVISORY_FILE.parent))
    parser.add_argument('--ca-file', default=os.environ.get('INSPECTION_ADVISORY_CA'))
    args = parser.parse_args()
    try:
        print(json.dumps(sync(args.url, args.public_key, args.root, args.ca_file)))
    except (ValueError, AttributeError, TypeError, KeyError, OSError, subprocess.SubprocessError) as error:
        print('advisory sync failed:', type(error).__name__, file=sys.stderr)
        sys.exit(1)
