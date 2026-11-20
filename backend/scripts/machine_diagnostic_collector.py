"""Linux host sampler. Run directly on the host with Python 3.10+ and systemd-detect-virt.

python3 machine_diagnostic_collector.py --binding /etc/doops/host.json --root /var/lib/doops/diagnostics
Example host-owned binding (SHA-256 of the local machine-id without its newline):
{"asset_id":"srv-01","cluster":"east","instance":"a","host_sha256":"<64 hex>",
 "applications":{"/system.slice/orders.service":{"system_id":"orders","namespace":"shop","application":"api"}}}
Expose this evidence tree read-only to the enrolled Agent at DOOPS_AGENT_MACHINE_DIAGNOSTICS_ROOT.
The existing Gateway-mounted mode uses DOOPS_MACHINE_DIAGNOSTICS_ROOT.
One sample per minute per asset; eight days of minute slots. The Gateway filters by exact time.
"""
import argparse
import configparser
import fcntl
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path


ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}')
NS = re.compile(r'[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?')


def read(path, size=8192):
    with path.open('rb') as stream:
        data = stream.read(size + 1)
    if len(data) > size:
        raise ValueError('host field exceeds size limit')
    return data.decode('utf-8')


def binding(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.geteuid()) or info.st_mode & 0o022 or info.st_size > 65536:
            raise ValueError('untrusted host binding')
        cfg = json.load(stream)
    if not all(ID.fullmatch(cfg[key]) for key in ('asset_id','cluster','instance')) or not re.fullmatch(r'[a-f0-9]{64}',cfg['host_sha256']):
        raise ValueError('invalid host binding')
    for group, app in cfg['applications'].items():
        if not group.startswith('/') or group == '/' or '..' in group.split('/') or set(app) != {'system_id','namespace','application'} or not (ID.fullmatch(app['system_id']) and NS.fullmatch(app['namespace']) and ID.fullmatch(app['application'])):
            raise ValueError('invalid cgroup deployment mapping')
    return cfg


def require_host():
    if sys.platform != 'linux' or not os.path.samefile('/', '/proc/1/root'):
        raise ValueError('host root and procfs required')
    for flag in ('--container', '--chroot'):
        p = subprocess.run(['/usr/bin/systemd-detect-virt', flag], stdin=subprocess.DEVNULL,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, env={'LC_ALL':'C'})
        if p.returncode != 1 or p.stderr or (flag == '--container' and p.stdout.strip() != b'none'):
            raise ValueError('cannot attest non-container host context')


def process(pid, apps, page_size):
    def fields():
        raw = read(pid / 'stat'); left, right = raw.index('('), raw.rindex(')')
        if raw[:left].strip() != pid.name:
            raise ValueError('process identity changed')
        tail = raw[right+1:].split()
        return raw[left+1:right], int(tail[19]), int(tail[21])
    name, start, rss = fields()
    group = [line[3:] for line in read(pid / 'cgroup').splitlines() if line.startswith('0::')]
    if fields()[1] != start:
        raise ProcessLookupError('process was replaced')
    item = {'pid':int(pid.name), 'start_ticks':start, 'rss_bytes':max(0,rss)*page_size,
            'name':name if re.fullmatch(r'[\w. +:/@-]{1,31}',name) else '[redacted]'}
    if len(group)==1 and group[0] in apps:
        item['deployment'] = apps[group[0]]
    return item


def sample(cfg, root, now_ms):
    machine_id = read(root / 'etc/machine-id',64).strip()
    if not re.fullmatch('[a-f0-9]{32}',machine_id) or machine_id == '0'*32 or hashlib.sha256(machine_id.encode()).hexdigest()!=cfg['host_sha256']:
        raise ValueError('host fingerprint mismatch')
    proc = root / 'proc'
    mounts = [line for line in read(proc/'self/mountinfo', 1<<20).splitlines() if ' - proc ' in line and line.split()[4]=='/proc']
    if len(mounts)!=1 or re.search(r'\bhidepid=(?!0\b|off\b)',mounts[0]):
        raise ValueError('host process visibility is incomplete')
    release = configparser.ConfigParser(interpolation=None,strict=False)
    release.read_string('[release]\n'+read(root/'etc/os-release',65536))
    os_id = release['release']['ID'].strip('"\''); version = release['release'].get('VERSION_ID','').strip('"\'')
    boot_id = str(uuid.UUID(read(proc/'sys/kernel/random/boot_id').strip()))
    stat_text = read(proc/'stat',4<<20)
    boot = re.search(r'^btime (\d+)$',stat_text,re.M)
    counters = next((line.split()[1:9] for line in stat_text.splitlines() if line.startswith('cpu ')), [])
    if len(counters) != 8 or not all(value.isdecimal() for value in counters):
        raise ValueError('invalid aggregate CPU counters')
    ticks = list(map(int, counters))  # guest time is already included in user/nice
    memory = dict(re.findall(r'^(MemTotal|MemAvailable):\s+(\d+) kB$', read(proc/'meminfo'), re.M))
    total, available = (int(memory[key]) * 1024 for key in ('MemTotal', 'MemAvailable'))
    if not 0 <= available <= total or total == 0:
        raise ValueError('invalid host memory counters')
    if boot is None:
        raise ValueError('boot time unavailable')
    oom = re.search(r'^oom_kill (\d+)$',read(proc/'vmstat',1<<20),re.M)
    processes, exited = [], 0
    page_size = os.sysconf('SC_PAGE_SIZE')
    for pid in proc.iterdir():
        if not re.fullmatch('[1-9][0-9]*',pid.name):
            continue
        try:
            processes.append(process(pid,cfg['applications'],page_size))
        except (FileNotFoundError, ProcessLookupError):
            exited += 1
    if not any(p['pid']==1 for p in processes) or len(processes)>32768:
        raise ValueError('incomplete or oversized host sample')
    return {k:cfg[k] for k in ('cluster','instance','asset_id','host_sha256')} | {
        'source':'host-procfs','at_ms':now_ms,'os':os_id,'version':version,'kernel':read(proc/'sys/kernel/osrelease').strip(),
        'cpu_total_ticks':sum(ticks),'cpu_idle_ticks':ticks[3]+ticks[4],
        'memory_total_bytes':total,'memory_used_bytes':total-available,
        'boot_id':boot_id,'boot_at':int(boot[1]),'oom_kills':int(oom[1]) if oom else None,'exited':exited,'processes':processes}


def publish(root, record):
    folder=root.resolve(strict=True)
    for key in ('cluster','instance','asset_id'):
        folder=folder/record[key]
        if folder.is_symlink():
            raise ValueError('invalid evidence directory')
        folder.mkdir(exist_ok=True)
    slot=(record['at_ms']//60000)%(8*1440)
    dest=folder/f'{slot}.json'
    fd=os.open(folder,os.O_RDONLY|os.O_DIRECTORY)
    try:
        fcntl.flock(fd,fcntl.LOCK_EX)
        if dest.exists() and json.loads(read(dest,8<<20))['at_ms']//60000 >= record['at_ms']//60000:
            return
        with tempfile.NamedTemporaryFile('w',dir=folder,delete=False) as stream:
            tmp=Path(stream.name)
            try:
                json.dump(record,stream,separators=(',',':'));stream.flush();os.fsync(stream.fileno());os.replace(tmp,dest)
            finally:
                tmp.unlink(missing_ok=True)
        os.fsync(fd)
    finally:
        os.close(fd)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__,formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--binding',type=Path,required=True);parser.add_argument('--root',type=Path,required=True)
    args=parser.parse_args()
    try:
        require_host();publish(args.root,sample(binding(args.binding),Path('/'),time.time_ns()//1_000_000))
    except (OSError,ValueError,KeyError,TypeError,IndexError,AttributeError,configparser.Error,subprocess.TimeoutExpired):
        parser.exit(1,'Host observation unavailable; check host context, binding and data access.\n')
