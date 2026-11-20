#!/usr/bin/env python3
"""Collect one observation for /v1/assets/monitor. Suitable for a system timer.

  asset_monitor_collector.py --cluster east --instance a --asset-id SRV-001 --mount /
  asset_monitor_collector.py --cluster east --instance a --pod-namespace orders

Default output is the typed JSON POST body. With --gateway https://gw.example,
read an explicitly provided bearer token from stdin and send the body directly.
A timer may run either command every minute, or use --interval-seconds 60.
Use --count N for a bounded run; Ctrl-C stops continuous mode. Each observation
reports HTTP failure through its exit status, and never retries a failed write.
Run machine/os/kernel probes directly on the identified host. Linux requires
/usr/bin/systemd-detect-virt and the host's root, network and mount namespaces.
Use --pod-namespace in an Agent container for Kubernetes Pod CPU and memory.
Repeat --mount for total, used and available bytes per exact mount point.
Repeat --interface for RX+TX bytes/s per Linux network interface. --nginx-status
reads a local nginx stub_status counter. Repeat --gpu for multiple NVIDIA cards.
Select GPUs by UUID for scheduled jobs. Numeric indices resolve once per invocation.
"""
import argparse
import http.client
import json
import os
import re
import subprocess
import sys
import time
import unicodedata
from functools import partial
from pathlib import Path
from urllib.parse import urlsplit


def proc_memory(field):
    for line in Path('/proc/meminfo').read_text().splitlines():
        if line.startswith(field + ':'):
            value, unit = line.split()[1:]
            if unit != 'kB':
                raise ValueError('unexpected /proc/meminfo unit')
            return int(value) * 1024
    raise ValueError('memory field unavailable')


def process_counts(args):
    # comm can contain spaces and ')'. Fields after its last ')' start at #3.
    fields = Path(f'/proc/{args.pid}/stat').read_text().rsplit(')', 1)[1].split()
    if int(fields[19]) != args.start_ticks:
        raise ValueError('process start time changed')
    return (int(fields[11]) + int(fields[12])) / os.sysconf('SC_CLK_TCK'), int(fields[21]) * os.sysconf('SC_PAGE_SIZE')


def cpu_time(kernel):
    values = list(map(int, Path('/proc/stat').read_text().splitlines()[0].split()[1:]))
    ticks = values[2] + sum(values[5:7])
    if not kernel:
        ticks += sum(values[:2])
    return ticks / os.sysconf('SC_CLK_TCK')


def network_bytes(interface):
    root = Path('/sys/class/net', interface, 'statistics')
    return sum(int((root / name).read_text()) for name in ('rx_bytes', 'tx_bytes'))


def require_host():
    # Darwin runs the filesystem probe natively. DoOps Agent containers run Linux.
    if sys.platform == 'darwin':
        return
    if sys.platform != 'linux' or not os.path.samefile('/', '/proc/1/root'):
        raise ValueError('native host required')
    for ns in ('mnt', 'net', 'pid'):
        if not os.path.samefile('/proc/self/ns/'+ns, '/proc/1/ns/'+ns):
            raise ValueError('host namespace required')
    for flag in ('--container', '--chroot'):
        result = subprocess.run(['/usr/bin/systemd-detect-virt', flag], capture_output=True,
                                timeout=5, env={'LC_ALL': 'C'})
        if result.returncode != 1 or result.stderr or (flag == '--container' and result.stdout.strip() != b'none'):
            raise ValueError('host context could not be verified')


def mount_bytes(mount):
    # Pin the filesystem while identifying and measuring it. A removed mount
    # must yield missing samples, even when its underlying directory still exists.
    fd = os.open(mount, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        if sys.platform == 'linux':
            info = Path(f'/proc/self/fdinfo/{fd}').read_text()
            match = re.search(r'^mnt_id:\s*(\d+)$', info, re.M)
            if not match:
                raise ValueError('mount ID unavailable')
            mount_id = match[1]
            escaped = re.sub(r'[ \\\t\n]', lambda m: r'\%03o' % ord(m[0]), mount)
            if not any(row[0] == mount_id and row[4] == escaped for row in
                       (line.split() for line in Path('/proc/self/mountinfo').read_text().splitlines())):
                raise ValueError('selected filesystem is not mounted here')
        elif not os.path.ismount(mount):
            raise ValueError('selected filesystem is not mounted here')
        stat = os.fstatvfs(fd)
        return (stat.f_blocks * stat.f_frsize, (stat.f_blocks - stat.f_bfree) * stat.f_frsize,
                stat.f_bavail * stat.f_frsize)
    finally:
        os.close(fd)


def nginx_count(url):
    con = http.client.HTTPConnection(url.hostname, url.port, timeout=3)
    try:
        con.request('GET', url.path or '/')
        response = con.getresponse()
        if response.status != 200:
            raise ValueError('stub_status unavailable')
        body = response.read(4097)
        match = re.fullmatch(rb'\s*Active connections:\s*\d+\s+server accepts handled requests\s+\d+\s+\d+\s+(\d+)\s+Reading:\s*\d+\s+Writing:\s*\d+\s+Waiting:\s*\d+\s*', body)
        if not match:
            raise ValueError('invalid stub_status')
        return int(match[1])
    finally:
        con.close()


GPU_UUID = re.compile(r'GPU-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}')


def nvidia_fields(device, fields):
    result = subprocess.check_output(['nvidia-smi', '-i', device, '--query-gpu='+fields,
                                      '--format=csv,noheader,nounits'], stderr=subprocess.DEVNULL, timeout=5, text=True)
    return [part.strip() for part in result.strip().split(',')]


def gpu_identity(selector):
    if GPU_UUID.fullmatch(selector):
        return selector
    if not re.fullmatch(r'[0-9]+', selector):
        raise ValueError('expected an NVIDIA device index or full GPU UUID')
    fields = nvidia_fields(selector, 'uuid')
    if len(fields) != 1 or not GPU_UUID.fullmatch(fields[0]):
        raise ValueError('cannot identify selected GPU')
    return fields[0]


def gpu_usage(uuid):
    identity, utilization = nvidia_fields(uuid, 'uuid,utilization.gpu')
    if identity != uuid:
        raise ValueError('GPU identity changed')
    value = int(utilization)
    if not 0 <= value <= 100:
        raise ValueError('invalid utilization')
    return value


def collect(args):
    route = {'cluster': args.cluster, 'instance': args.instance}
    if args.pod_namespace:
        return dict(route, action='collect', namespace=args.pod_namespace)
    probes = {}
    if args.host:
        probes['cpu', ''] = ('cores', True, lambda: cpu_time(args.type == 'kernel'))
        if args.type == 'kernel':
            probes['memory', ''] = ('bytes', False, lambda: proc_memory('Slab'))
        else:
            probes['memory', ''] = ('bytes', False, lambda: proc_memory('MemTotal') - proc_memory('MemAvailable'))
    if args.pid:
        probes['cpu', ''] = ('cores', True, lambda: process_counts(args)[0])
        probes['memory', ''] = ('bytes', False, lambda: process_counts(args)[1])
    if args.nginx_status:
        probes['requests', ''] = ('req/s', True, lambda: nginx_count(args.nginx_status))
    for interface in args.interface:
        probes['network', interface] = ('bytes/s', True, partial(network_bytes, interface))

    def observed(metric, read):
        try:
            return read(), time.monotonic()
        except (OSError, ValueError, IndexError, subprocess.SubprocessError, http.client.HTTPException):
            print(f'{metric}: measurement unavailable', file=sys.stderr)
            return None, time.monotonic()

    prior = {k: observed(k, read) for k, (_, rate, read) in probes.items() if rate}
    if prior:
        time.sleep(1)
    samples = []
    def append(metric, device, unit, value, at_ms):
        sample = dict(type=args.type, asset_id=args.asset_id, namespace=args.namespace, metric=metric,
                      unit=unit, at_ms=at_ms, status='ok' if value is not None else 'missing', value=value)
        if device:
            sample['device_id'] = device
        samples.append(sample)

    for key, (unit, rate, read) in probes.items():
        value, tick = observed(key, read)
        if rate:
            old, start = prior[key]
            value = (value-old)/(tick-start) if old is not None and value is not None and value >= old else None
        append(*key, unit, value, time.time_ns()//1000000)
    for mount in args.mount:
        sizes, _ = observed(mount, partial(mount_bytes, mount))
        at_ms = time.time_ns()//1000000
        for metric, value in zip(('storage_total', 'storage', 'storage_available'),
                                 sizes if sizes is not None else (None, None, None)):
            append(metric, mount, 'bytes', value, at_ms)
    for uuid in args.gpu:
        value, _ = observed('gpu '+uuid, lambda: gpu_usage(uuid))
        append('gpu', uuid, '%', value, time.time_ns()//1000000)
    return dict(route, action='ingest', samples=samples)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for name in ('cluster','instance'):
        ap.add_argument('--'+name, required=True)
    ap.add_argument('--pod-namespace')
    ap.add_argument('--asset-id')
    ap.add_argument('--type', choices=['machine','os','kernel','process','app','database'], default='machine')
    ap.add_argument('--namespace', default='')
    ap.add_argument('--host', action='store_true', help='Linux host CPU and memory; kernel uses system/IRQ CPU and Slab bytes')
    ap.add_argument('--pid', type=int)
    ap.add_argument('--start-ticks', type=int, help='Expected /proc/PID/stat field 22')
    ap.add_argument('--interface', action='append', default=[], help='Linux network interface; repeat per device')
    ap.add_argument('--mount', action='append', default=[], help='Absolute mount point; repeat per filesystem')
    ap.add_argument('--gpu', action='append', default=[], help='NVIDIA index or full GPU UUID; repeat for multiple cards')
    ap.add_argument('--nginx-status', type=urlsplit, help='Explicit HTTP loopback stub_status URL')
    ap.add_argument('--gateway', type=urlsplit, help='Explicit HTTPS Gateway origin. Bearer supplied on stdin.')
    ap.add_argument('--interval-seconds', type=int, help='Run periodically, every 1–300 seconds')
    ap.add_argument('--count', type=int, help='Maximum number of observations in periodic mode')
    args = ap.parse_args()
    if args.interval_seconds is not None and not 1 <= args.interval_seconds <= 300:
        ap.error('interval must be 1–300 seconds')
    if args.count is not None and (not args.interval_seconds or args.count < 1):
        ap.error('--count requires periodic mode and a positive count')
    probes = any((args.host,args.pid,args.interface,args.mount,bool(args.gpu),args.nginx_status))
    if args.pod_namespace and (args.asset_id or probes) or not args.pod_namespace and not (args.asset_id and probes):
        ap.error('select --pod-namespace or --asset-id with at least one probe')
    if args.host and args.type not in ('machine','os','kernel') or args.pid is not None and (args.type not in ('process','app','database') or args.pid < 1 or args.start_ticks is None):
        ap.error('use --host for machine/os/kernel, or --pid with --start-ticks for process/app/database')
    if any(not re.fullmatch('[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}', dev) for dev in args.interface):
        ap.error('invalid interface name')
    for mount in args.mount:
        if (not mount.startswith('/') or mount.startswith('//') or mount != os.path.normpath(mount)
                or len(os.fsencode(mount)) >= 4096 or any(unicodedata.category(c) == 'Cc' for c in mount)):
            ap.error('mount must be a canonical absolute path')
    if len(set(args.mount)) != len(args.mount) or len(set(args.interface)) != len(args.interface):
        ap.error('duplicate mount or interface selected')
    if not args.pod_namespace and args.type in ('machine', 'os', 'kernel'):
        try:
            require_host()
        except (OSError, ValueError, subprocess.SubprocessError):
            ap.error('machine/os/kernel observations require a verified native host; check host namespaces and systemd-detect-virt')
    url = args.nginx_status
    if url and (url.scheme != 'http' or url.hostname not in ('127.0.0.1','::1') or url.username or url.password or url.query or url.fragment):
        ap.error('stub_status must be an explicit HTTP loopback address')
    gw = args.gateway
    if gw and (gw.scheme != 'https' or not gw.hostname or gw.username or gw.password or gw.path not in ('','/') or gw.query or gw.fragment):
        ap.error('gateway must be an HTTPS origin')
    token = sys.stdin.readline(16385).strip() if gw else ''
    if gw and (not token or len(token)>16384 or any(ord(c)<=32 or ord(c)>=127 for c in token)):
        ap.error('supply a bearer token via stdin')
    try:
        args.gpu = [gpu_identity(selector) for selector in args.gpu]
    except (OSError, ValueError, subprocess.SubprocessError):
        ap.error('could not resolve the selected GPU to a stable UUID')
    if len(set(args.gpu)) != len(args.gpu):
        ap.error('duplicate GPU UUID selected')
    count = 0
    while True:
        started = time.monotonic()
        body = json.dumps(collect(args), allow_nan=False)
        if gw:
            submit(gw, token, body)
        else:
            print(body, flush=True)
        count += 1
        if not args.interval_seconds or count == args.count:
            return
        time.sleep(max(0, args.interval_seconds - (time.monotonic() - started)))


def submit(gw, token, body):
    con = http.client.HTTPSConnection(gw.hostname,gw.port,timeout=40)
    try:
        con.request('POST','/v1/assets/monitor',body,{'Content-Type':'application/json','Authorization':'Bearer '+token})
        response = con.getresponse()
        if response.status != 200:
            raise SystemExit(f'Gateway rejected collection: HTTP {response.status}')
        result = json.loads(response.read(4096))
        print(f"accepted {int(result['accepted'])} samples")
    finally:
        con.close()


if __name__ == '__main__':
    main()
