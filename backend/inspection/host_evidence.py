"""Publish one local Linux host observation per configured inspection check."""
import argparse
import fcntl
import hashlib
import json
import math
import os
import re
import stat
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path


class Gap(ValueError):
    """Safe diagnostic suitable for an operator's local log."""


def positive(value, maximum):
    if type(value) is not int or not 0 < value <= maximum:
        raise Gap('invalid numeric policy')
    return value


def names(value, pattern):
    if not isinstance(value, list) or not value or not all(isinstance(v, str) and re.fullmatch(pattern, v) for v in value):
        raise Gap('missing or invalid policy names')
    return set(value)


def text(path, limit):
    with path.open('rb') as stream:
        raw = stream.read(limit + 1)
    if len(raw) > limit:
        raise Gap('input exceeds bound')
    return raw.decode('utf-8')


def load_policy(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.geteuid()) or info.st_mode & 0o022:
            raise Gap('untrusted policy file')
        data = stream.read(65537)
        if len(data) > 65536:
            raise Gap('policy exceeds bound')
        return json.loads(data)


def utc(epoch):
    return datetime.fromtimestamp(epoch, timezone.utc).isoformat()


def boot(root, policy, now):
    threshold = positive(policy['min_uptime_seconds'], 10**9)
    stat = text(root / 'proc/stat', 4 << 20)
    starts = re.findall(r'^btime (\d+)$', stat, re.M)
    fields = text(root / 'proc/uptime', 128).split()
    if len(starts) != 1 or len(fields) != 2:
        raise Gap('incomplete proc boot data')
    started, uptime = int(starts[0]), float(fields[0])
    if not math.isfinite(uptime) or uptime < 0 or started < 1 or abs(now - started - uptime) > 5:
        raise Gap('inconsistent boot time and uptime')
    return now, 'linux-proc-boot', f'minimum uptime {threshold} seconds', 1, int(uptime < threshold), f'boot={utc(started)} uptime={int(uptime)}s'


def processes(root, policy, now):
    blocked = names(policy['denied_comm'], r'[^\s/\x00-\x1f\x7f]{1,15}')
    if any(len(v.encode()) > 15 for v in blocked):
        raise Gap('comm exceeds kernel name limit')
    proc = root / 'proc'
    mounts = [line for line in text(proc / 'self/mountinfo', 1 << 20).splitlines()
              if len(line.split()) >= 10 and line.split()[4] == '/proc' and ' - proc ' in line]
    if len(mounts) != 1 or re.search(r'\bhidepid=(?!0\b|off\b)', mounts[0]):
        raise Gap('incomplete proc visibility')
    pids = [entry for entry in proc.iterdir() if entry.name.isascii() and entry.name.isdigit()]
    if not any(p.name == '1' for p in pids):
        raise Gap('PID 1 is outside proc view')
    hits = 0
    for pid in pids:
        comm = text(pid / 'comm', 64).removesuffix('\n')
        if not comm:
            raise Gap('empty process name')
        hits += comm in blocked
    return now, 'linux-proc-comm', 'exact process name denylist', len(pids), hits, f'observed_pids={len(pids)} listed_names={len(blocked)} matched={hits}'


SYSCTLS = {'kernel.randomize_va_space': {0, 1, 2}, 'kernel.kptr_restrict': {0, 1, 2},
           'fs.protected_hardlinks': {0, 1}, 'fs.protected_symlinks': {0, 1}, 'net.ipv4.ip_forward': {0, 1}}


def configuration(root, policy, now):
    baseline = policy['sysctl']
    if not isinstance(baseline, dict) or not baseline:
        raise Gap('missing sysctl baseline')
    details, hits = [], 0
    for key, expected in sorted(baseline.items()):
        if key not in SYSCTLS or type(expected) is not int or expected not in SYSCTLS[key]:
            raise Gap('unsupported baseline rule')
        actual = int(text((root / 'proc/sys').joinpath(*key.split('.')), 32).strip())
        hits += actual != expected
        details.append(f'{key}={actual} expected={expected}')
    return now, 'linux-proc-sys', 'exact local sysctl baseline', len(details), hits, ', '.join(details)


AUDIT_HEADER = re.compile(rb'^type=([A-Z0-9_]+) msg=audit\((\d+\.\d+):(\d+)\):')
RESULT = re.compile(rb"\bres=(success|failed)\b")
GROUP = re.compile(rb'''\b(?:grp|new_group|new-grp)=(?:"([a-zA-Z0-9_.-]+)"|([a-zA-Z_][a-zA-Z0-9_.-]*))(?=[ '\"]|$)''')
GROUP_TYPES = {b'GRP_MGMT', b'ADD_GROUP', b'DEL_GROUP', b'GRP_CHAUTHTOK'}


def audit_tail(path):
    # Bound memory and ignore only a leading fragment outside the provable window.
    limit = 8 << 20
    with path.open('rb') as stream:
        size = stream.seek(0, os.SEEK_END)
        offset = max(0, size - limit)
        stream.seek(offset)
        data = stream.read(size - offset)
        if len(data) != size - offset or not data.endswith(b'\n'):
            raise Gap('truncated audit log')
    lines = data.splitlines()
    return lines[1:] if offset else lines


def logins(root, policy, now):
    window = positive(policy['window_seconds'], 86400)
    gap = positive(policy['max_gap_seconds'], window)
    privileged = {s.encode() for s in names(policy['privileged_groups'], r'[a-zA-Z_][a-zA-Z0-9_.-]{0,31}')}
    # Leave one sampling interval for a real trailing record to bracket the window.
    end = int(now) - gap
    start = end - window
    previous = None
    bracketed = False
    login_count = group_count = denied = group_changes = 0
    for line in audit_tail(root / 'var/log/audit/audit.log'):
        header = AUDIT_HEADER.match(line)
        if not header:
            raise Gap('unrecognized or nonlocal audit record')
        kind, raw_at, raw_serial = header.groups()
        at, serial = float(raw_at), int(raw_serial)
        if previous and at < previous[0]:
            raise Gap('out of order audit log')
        if at > now:
            raise Gap('future audit record')
        if at >= start and not bracketed:
            if previous is None:
                raise Gap('audit window start missing')
            last_at, last_serial = previous
            if at - last_at > gap or serial not in (last_serial, last_serial + 1) or (serial == last_serial and at != last_at):
                raise Gap('audit sequence or time gap')
            if kind.startswith((b'DAEMON_', b'SYSTEM_BOOT', b'SYSTEM_SHUTDOWN')) or kind == b'CONFIG_CHANGE' or re.search(rb'\blost=[1-9]', line):
                raise Gap('audit collection interrupted or configuration changed')
        previous = at, serial
        if at >= end:
            bracketed = True
        if at < start or bracketed:
            continue
        login = kind in (b'USER_LOGIN', b'USER_AUTH')
        group = kind in GROUP_TYPES or (kind == b'USER_MGMT' and (b'group' in line or b'grp=' in line))
        if not (login or group):
            continue
        # Only inspect the PAM/shadow audit result and group. Never export raw text.
        results = RESULT.findall(line[header.end():])
        if len(results) != 1:
            raise Gap('ambiguous authentication or group audit result')
        if login:
            login_count += 1
            denied += results[0] == b'failed'
        else:
            groups = GROUP.findall(line[header.end():])
            if not groups:
                raise Gap('group audit record lacks a readable group')
            group_count += 1
            group_changes += results[0] == b'success' and any((a or b) in privileged for a, b in groups)
    if not bracketed or not login_count or not group_count:
        raise Gap('missing window boundary, login stream or group stream')
    detail = f'[{utc(start)}, {utc(end)}) auth_records={login_count} denied={denied} group_records={group_count} privileged_changes={group_changes}'
    return end, 'linux-audit-local', 'denied login/auth and successful privileged group operations', login_count + group_count, denied + group_changes, detail


CHECKS = {'M1': boot, 'M3': processes, 'M4': configuration, 'M5': logins}


def publish(path, record):
    with tempfile.NamedTemporaryFile(mode='w', encoding='utf-8', dir=path.parent, delete=False) as stream:
        temp = Path(stream.name)
        try:
            json.dump(record, stream, ensure_ascii=True, allow_nan=False)
            stream.flush()
            os.fsync(stream.fileno())
            os.replace(temp, path)
        finally:
            temp.unlink(missing_ok=True)


def collect(subject, evidence_root, policy_file, *, root=Path('/'), now=None):
    parts = [subject[k] for k in ('cluster', 'instance', 'asset_id')]
    if not all(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', p) for p in parts):
        raise Gap('invalid asset identifier')
    folder = evidence_root.resolve(strict=True)
    for part in parts:
        folder = folder / part
        if folder.is_symlink():
            raise Gap('symlink in output path')
        folder.mkdir(exist_ok=True)
    lock = os.open(folder, os.O_RDONLY | os.O_DIRECTORY)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            policies = load_policy(policy_file)
        except (OSError, ValueError, RecursionError):
            policies = None
        gaps = {}
        for check, observe in CHECKS.items():
            path = folder / (check + '.json')
            try:
                rules = policies[check]
                at, source, policy, samples, violations, detail = observe(root, rules, time.time() if now is None else now)
                digest = hashlib.sha256(json.dumps(rules, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
            except (OSError, ValueError, KeyError, TypeError, OverflowError) as exc:
                path.unlink(missing_ok=True)
                gaps[check] = str(exc) if isinstance(exc, Gap) else 'missing or unreadable source/policy'
                continue
            record = {**subject, 'check_id': check, 'observed_at': utc(at), 'source': source, 'policy': f'{policy} #{digest}',
                      'samples': samples, 'violations': violations, 'detail': detail}
            try:
                publish(path, record)
            except OSError:
                path.unlink(missing_ok=True)
                raise
        os.fsync(lock)
        return gaps
    finally:
        os.close(lock)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for arg in ('cluster', 'instance', 'asset-id'):
        parser.add_argument('--' + arg, required=True)
    parser.add_argument('--policy', required=True, type=Path)
    parser.add_argument('--evidence-root', required=True, type=Path)
    args = parser.parse_args()
    subject = {k: getattr(args, k) for k in ('asset_id', 'cluster', 'instance')}
    gaps = collect(subject, args.evidence_root, args.policy)
    print(json.dumps({'gaps': gaps, 'published': sorted(CHECKS.keys() - gaps.keys())}))
    return int(bool(gaps))


if __name__ == '__main__':
    sys.exit(main())
