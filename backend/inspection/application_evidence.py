"""Publish A1–A5 receipts for one explicitly configured asset (POSIX, Python 3.10+)."""
import argparse
import fcntl
import http.client
import json
import math
import os
import re
import signal
import ssl
import stat
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit


SUBJECT = ('cluster', 'instance', 'asset_id')
POLICIES = {
    'A1': 'service health HTTP status',
    'A2': 'database_up equals 1',
    'A3': 'requests_per_second within configured inclusive bounds',
    'A4': 'verified HTTPS, positive HSTS max-age and nosniff',
    'A5': 'API HTTP status and typed JSON field equality',
}
FIELDS = {'A1': {'status'}, 'A2': set(), 'A3': {'min', 'max'}, 'A4': set(), 'A5': {'status', 'field', 'equals'}}
TIMEOUT = 5
BODY_LIMIT = 64 * 1024
FRESH_SECONDS = 300


class Gap(Exception):
    """A public, fixed reason for an unavailable measurement."""


def validate(condition):
    if not condition:
        raise ValueError('invalid input')


def parse_json(raw):
    def unique(pairs):
        result = dict(pairs)
        validate(len(result) == len(pairs))
        return result

    def invalid(_):
        raise ValueError('invalid number')

    return json.loads(raw.decode('utf-8'), object_pairs_hook=unique, parse_constant=invalid)


def number(v):
    return type(v) in (int, float) and math.isfinite(v)


def endpoint(value):
    validate(isinstance(value, str) and 0 < len(value) <= 1024 and value.isascii())
    validate(not re.search(r'[\x00-\x20\x7f\\?#@%]', value))
    url = urlsplit(value)
    validate(url.scheme in ('http', 'https') and url.hostname and url.port != 0)
    validate(re.fullmatch(r'[a-zA-Z0-9.:-]+', url.hostname) and
             re.fullmatch(r'[/a-zA-Z0-9_.~-]*', url.path) and '..' not in url.path.split('/'))
    validate(url.scheme == 'https' or url.hostname in ('127.0.0.1', '::1', 'localhost'))
    return url


def load_config(path):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        st = os.fstat(stream.fileno())
        validate(stat.S_ISREG(st.st_mode) and st.st_uid in (0, os.geteuid()) and not st.st_mode & 0o022)
        raw = stream.read(32769)
    validate(len(raw) <= 32768)
    config = parse_json(raw)
    validate(type(config) is dict and set(config) == {*SUBJECT, 'checks'})
    for key in SUBJECT:
        validate(isinstance(config[key], str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', config[key]))
    checks = config['checks']
    validate(type(checks) is dict and 0 < len(checks) <= 5 and checks.keys() <= POLICIES.keys())
    return config


def validate_rule(check, rule):
    validate(type(rule) is dict and set(rule) == {'url'} | FIELDS[check])
    url = endpoint(rule['url'])
    if check in ('A1', 'A5'):
        validate(type(rule['status']) is int and 200 <= rule['status'] < 300)
    if check == 'A3':
        validate(number(rule['min']) and number(rule['max']) and 0 <= rule['min'] <= rule['max'])
    if check == 'A5':
        keys, value = rule['field'], rule['equals']
        validate(type(keys) is list and 1 <= len(keys) <= 8 and all(isinstance(k, str) and re.fullmatch(r'[A-Za-z0-9_-]{1,64}', k) for k in keys))
        validate(type(value) is bool or number(value) or (isinstance(value, str) and len(value) <= 128))
    return url


def deadline(_signum, _frame):
    raise TimeoutError('probe deadline')


def get(url, json_statuses):
    # Direct socket connections send neither environment proxy credentials nor cookies.
    factory = http.client.HTTPSConnection if url.scheme == 'https' else http.client.HTTPConnection
    connection = factory(url.hostname, url.port, timeout=TIMEOUT)
    previous = signal.signal(signal.SIGALRM, deadline)
    signal.setitimer(signal.ITIMER_REAL, TIMEOUT)
    try:
        connection.request('GET', url.path or '/', headers={'Accept-Encoding': 'identity', 'Cache-Control': 'no-cache'})
        with connection.getresponse() as response:
            status = response.status
            if status in (401, 403, 407):
                raise Gap('permission_denied')
            if 300 <= status < 400:
                raise Gap('redirect_not_followed')
            if status == 429:
                raise Gap('rate_limited')
            headers = response.headers
            if sum(len(k) + len(v) for k, v in headers.items()) > 16384:
                raise Gap('response_too_large')
            body = None
            if status in json_statuses:
                if headers.get_content_type() != 'application/json' or headers.get('Content-Encoding', 'identity').lower() != 'identity':
                    raise Gap('unsupported_response')
                length = headers.get('Content-Length')
                if length is not None and (not re.fullmatch('[0-9]{1,18}', length) or len(headers.get_all('Content-Length')) != 1):
                    raise Gap('invalid_response')
                if length is not None and int(length) > BODY_LIMIT:
                    raise Gap('response_too_large')
                raw = response.read(BODY_LIMIT + 1)
                if len(raw) > BODY_LIMIT:
                    raise Gap('response_too_large')
                if length is not None and len(raw) != int(length):
                    raise Gap('incomplete_response')
                body = parse_json(raw)
            return status, headers, body
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)
        connection.close()


def exporter(check, rule, data, subject, received):
    metric, unit = ('database_up', '1') if check == 'A2' else ('requests_per_second', 'req/s')
    if not isinstance(data, dict) or any(data.get(k) != v for k, v in subject.items()) or (data.get('metric'), data.get('unit')) != (metric, unit):
        raise Gap('exporter_subject_or_metric_mismatch')
    value = data.get('value')
    if not number(value) or value < 0 or (check == 'A2' and value not in (0, 1)):
        raise Gap('invalid_exporter_scalar')
    at = data['observed_at']
    validate(isinstance(at, str) and re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,6})?(?:Z|[+-]\d\d:\d\d)', at))
    observed = datetime.fromisoformat(at.replace('Z', '+00:00'))
    if not 0 <= (received - observed).total_seconds() <= FRESH_SECONDS:
        raise Gap('stale_or_future_exporter_sample')
    low, high = (1, 1) if check == 'A2' else (rule['min'], rule['max'])
    return {'observed_at': observed.isoformat(), 'samples': 1, 'violations': int(not low <= value <= high),
            'detail': f'{metric}={value:.12g} {unit}, allowed=[{low:.12g},{high:.12g}] {unit}, exporter point sample'}


def measurement(check, rule, url, subject):
    json_statuses = ()
    if check in ('A2', 'A3'):
        json_statuses = range(200, 300)
    elif check == 'A5':
        json_statuses = (rule['status'],)
    status, headers, body = get(url, json_statuses)
    if check in ('A1', 'A5') and status != rule['status']:
        return {'samples': 1, 'violations': 1, 'detail': f'HTTP {status}, expected {rule["status"]}'}
    if not 200 <= status < 300:
        raise Gap('source_http_unavailable')
    if check == 'A1':
        return {'samples': 1, 'violations': 0, 'detail': f'HTTP {status}, expected {rule["status"]}'}
    if check in ('A2', 'A3'):
        return exporter(check, rule, body, subject, datetime.now(timezone.utc))
    if check == 'A4':
        hsts = headers.get_all('Strict-Transport-Security', [])
        ages = [s.strip() for s in hsts[0].split(';') if s.strip().lower().split('=', 1)[0].strip() == 'max-age'] if len(hsts) == 1 else []
        match = re.fullmatch(r'max-age\s*=\s*("?)([0-9]{1,12})\1', ages[0], re.I) if len(ages) == 1 else None
        valid_hsts = bool(match and int(match[2]) > 0)
        nosniff = [v.strip().lower() for v in headers.get_all('X-Content-Type-Options', [])] == ['nosniff']
        tls = url.scheme == 'https'
        return {'samples': 3, 'violations': 3 - sum([tls, valid_hsts, nosniff]),
                'detail': f'verified_https={int(tls)}, positive_hsts={int(valid_hsts)}, nosniff={int(nosniff)}'}
    value = body
    for key in rule['field']:
        if not isinstance(value, dict) or key not in value:
            return {'samples': 1, 'violations': 1, 'detail': f'HTTP {status}, required field absent'}
        value = value[key]
    expected = rule['equals']
    same_type = type(value) is type(expected) or (number(value) and number(expected))
    matches = same_type and value == expected
    return {'samples': 1, 'violations': int(not matches), 'detail': f'HTTP {status}, expected field matched={int(matches)}'}


def collect(check, rule, subject):
    try:
        try:
            url = validate_rule(check, rule)
        except (ValueError, TypeError, OverflowError):
            raise Gap('invalid_probe_policy') from None
        result = measurement(check, rule, url, subject)
    except Gap as error:
        result = {'gap': error.args[0]}
    except ssl.SSLCertVerificationError:
        result = ({'samples': 1, 'violations': 1, 'detail': 'TLS peer certificate verification failed'}
                  if check == 'A4' else {'gap': 'tls_verification_failed'})
    except TimeoutError:
        result = {'gap': 'probe_timeout'}
    except (OSError, http.client.HTTPException):
        result = {'gap': 'source_unreachable'}
    except (ValueError, TypeError, KeyError, OverflowError, RecursionError):
        result = {'gap': 'invalid_response'}
    return {**subject, 'check_id': check, 'observed_at': datetime.now(timezone.utc).isoformat(),
            'source': 'application-exporter' if check in ('A2', 'A3') else 'application-http-probe',
            'policy': POLICIES[check], **result}


def publish(folder, record):
    fd, name = tempfile.mkstemp(prefix='.application-', dir=folder)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as out:
            json.dump(record, out, allow_nan=False)
            os.fchmod(out.fileno(), 0o640)
            out.flush()
            os.fsync(out.fileno())
        os.replace(name, folder / (record['check_id'] + '.json'))
    finally:
        Path(name).unlink(missing_ok=True)


def run(config_path, root):
    config = load_config(config_path)
    subject = {k: config[k] for k in SUBJECT}
    folder = root.resolve(strict=True)
    for k in SUBJECT:
        folder = folder / subject[k]
        folder.mkdir(mode=0o750, exist_ok=True)
        validate(not folder.is_symlink() and folder.is_dir())
    lock = os.open(folder, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        totals = {'published': 0, 'findings': 0, 'gaps': 0}
        for check, rule in config['checks'].items():
            record = collect(check, rule, subject)
            publish(folder, record)
            os.fsync(lock)
            totals['published'] += 1
            totals['gaps'] += int('gap' in record)
            totals['findings'] += int(record.get('violations', 0) > 0)
        return totals
    finally:
        os.close(lock)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True, type=Path)
    parser.add_argument('--root', required=True, type=Path)
    args = parser.parse_args()
    try:
        totals = run(args.config, args.root)
        print(json.dumps(totals))
        sys.exit(2 if totals['gaps'] else 0)
    except (ValueError, OSError, TypeError, OverflowError, RecursionError) as error:
        print('application evidence publication failed:', type(error).__name__, file=sys.stderr)
        sys.exit(1)
