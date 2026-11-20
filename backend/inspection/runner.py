"""One bounded polling iteration, launched by the executor's external timer."""
import argparse
import hashlib
import json
import os
import platform
import re
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import quote, urlencode, urlsplit, unquote
from urllib.request import Request, HTTPRedirectHandler, build_opener


DEFAULT_ADVISORY_FILE = Path('/var/lib/doops-advisories/advisories.json')
MAX_ARTIFACT = 8 * 1024 * 1024


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def timestamp():
    return datetime.now(timezone.utc).isoformat()


def epoch(value):
    if not isinstance(value, str):
        raise ValueError('expected timestamp string')
    instant = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if instant.tzinfo is None:
        raise ValueError('timestamp lacks timezone')
    return instant.timestamp()


def bounded(stream, limit):
    raw = stream.read(limit + 1)
    if len(raw) > limit:
        raise ValueError('input too large')
    return raw


def artifact(path):
    with path.open('rb') as stream:
        raw = bounded(stream, MAX_ARTIFACT)
    return json.loads(raw), hashlib.sha256(raw).hexdigest()


def receipt(folder, item):
    record, digest = artifact(folder / (item['check_id'] + '.json'))
    for k in ('asset_id', 'cluster', 'instance', 'check_id'):
        if record[k] != item[k]:
            raise ValueError('wrong receipt subject')
    if 'gap' in record:
        gap = record['gap']
        if not isinstance(gap, str) or not re.fullmatch(r'[a-z][a-z0-9_]{0,63}', gap) or 'violations' in record or 'samples' in record:
            raise ValueError('invalid gap receipt')
        return {k: record[k] for k in ('observed_at', 'source', 'gap')} | {'sha256': digest}
    return {k: record[k] for k in ('observed_at', 'source', 'policy', 'detail', 'samples', 'violations')} | {'sha256': digest}


def asset_parts(subject):
    parts = [subject[k] for k in ('cluster', 'instance', 'asset_id')]
    if not all(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', part) for part in parts):
        raise ValueError('invalid asset path')
    return parts


def collect_dpkg(root, subject):
    parts = asset_parts(subject)
    release = platform.freedesktop_os_release()
    distro, release_id = release['ID'], release['VERSION_ID']
    if distro not in ('debian', 'ubuntu') or not re.fullmatch(r'[0-9]+(?:\.[0-9]+)*', release_id):
        raise ValueError('expected a Debian or Ubuntu release with a numeric VERSION_ID')
    observed = timestamp()
    fields = r'${db:Status-Status}\t${Package}\t${Version}\t${Architecture}\n'
    query = subprocess.run(['dpkg-query', '-W', '-f=' + fields], capture_output=True, text=True,
                           check=True, timeout=30, env=dict(os.environ, LC_ALL='C'))
    components = []
    for row in query.stdout.splitlines():
        state, name, version, arch = row.split('\t')
        if state == 'installed':
            if not name or not version or not arch:
                raise ValueError('incomplete installed package')
            qualifiers = urlencode({'arch': arch, 'distro': f'{distro}-{release_id}'})
            purl = f'pkg:deb/{distro}/{quote(name, safe="")}@{quote(version, safe="")}?{qualifiers}'
            components.append({'type': 'library', 'name': name, 'version': version, 'purl': purl})
    if not components:
        raise ValueError('empty installed package inventory')
    properties = [{'name': 'doops:' + k, 'value': v} for k, v in subject.items()]
    bom = {'bomFormat': 'CycloneDX', 'specVersion': '1.5', 'version': 1,
           'metadata': {'timestamp': observed, 'properties': properties}, 'components': components}
    raw = json.dumps(bom, separators=(',', ':')).encode()
    if len(raw) > MAX_ARTIFACT:
        raise ValueError('SBOM exceeds runner input limit')
    folder = root.resolve(strict=True)
    for part in parts:
        folder = folder / part
        if folder.is_symlink():
            raise ValueError('symlink in SBOM output path')
        folder.mkdir(mode=0o750, exist_ok=True)
    dest = folder / 'sbom.json'
    with tempfile.NamedTemporaryFile(dir=folder, delete=False) as out:
        temp = Path(out.name)
        try:
            out.write(raw)
            out.flush()
            os.fchmod(out.fileno(), 0o640)
            os.fsync(out.fileno())
            os.replace(temp, dest)
        finally:
            temp.unlink(missing_ok=True)
    return {'path': str(dest), 'components': len(components), 'sha256': hashlib.sha256(raw).hexdigest()}


def software(folder, advisory_file, item):
    bom, digest = artifact(folder / 'sbom.json')
    feed, bulletin = artifact(advisory_file)
    if bom['bomFormat'] != 'CycloneDX' or not epoch(feed['issued_at']) <= time.time() < epoch(feed['valid_until']):
        raise ValueError('invalid inventory or advisory validity')
    metadata = bom['metadata']
    props = {p['name']: p['value'] for p in metadata['properties']}
    if any(props.get('doops:' + k) != item[k] for k in ('asset_id', 'cluster', 'instance')):
        raise ValueError('wrong SBOM subject')
    deployed = {}
    pending = list(bom['components'])
    while pending:
        component = pending.pop()
        pending.extend(component.get('components', []))
        purl, version = component['purl'], component['version']
        base, sep, tail = re.split(r'([?#])', purl, maxsplit=1) if re.search(r'[?#]', purl) else (purl, '', '')
        pkg, at, embedded = base.rpartition('@')
        if at and '/' not in embedded:
            if unquote(embedded) != version:
                raise ValueError('package version mismatch')
            base = pkg
        identity = base + sep + tail
        if identity in deployed and deployed[identity] != version:
            raise ValueError('ambiguous package version')
        deployed[identity] = version
    kinds = {'V1': ['vulnerability'], 'V2': ['patch'], 'V3': ['vulnerability', 'patch']}[item['check_id']]
    packages = []
    for rule in feed['advisories']:
        pkg = rule['component']
        if rule['kind'] in kinds and pkg in deployed:
            packages.append({k: rule[k] for k in ('component','advisory','introduced','fixed')} | {'version': deployed[pkg]})
    return {'source':'deployed-cyclonedx', 'observed_at':metadata['timestamp'], 'sha256':digest, 'bulletin_sha256':bulletin, 'packages':packages}


def machine_metrics(api, item):
    params = {k:item[k] for k in ('cluster','instance','asset_id')} | {'type':'machine','origin':'exporter','range':'1h'}
    result = api('/v1/assets/monitor?' + urlencode(params))
    readings = {s['metric']:s for s in result['series']}
    rows, observed, violations = [], [], 0
    for metric in ('cpu','memory'):
        series = readings[metric]
        point, rule = series['latest'], series['rule']
        if series['stale'] or point['status'] != 'ok' or rule is None:
            raise ValueError('missing fresh monitor sample or rule')
        observed.append(point['at_ms']/1000)
        violations += int(point['value'] > rule['threshold'])
        rows.append(f"{metric}={point['value']} {series['unit']}, threshold={rule['threshold']}")
    raw = json.dumps(result, sort_keys=True, separators=(',',':')).encode()
    return {'source':'gateway-monitor-exporter','sha256':hashlib.sha256(raw).hexdigest(),'observed_at':datetime.fromtimestamp(min(observed),timezone.utc).isoformat(),'samples':2,'violations':violations,'policy':'point-in-time configured CPU and memory thresholds','detail':' / '.join(rows)}


def run():
    gateway = os.environ['INSPECTION_GATEWAY'].rstrip('/')
    url = urlsplit(gateway)
    if url.username or url.password or url.query or url.fragment or url.path or not (url.scheme == 'https' or url.scheme == 'http' and url.hostname in ('localhost','127.0.0.1','::1')):
        raise ValueError('use an HTTPS gateway origin or loopback for local tests')
    token = Path(os.environ['INSPECTION_TOKEN_FILE']).read_text().strip()
    root = Path(os.environ['INSPECTION_EVIDENCE_ROOT']).resolve(strict=True)
    advisory_file = Path(os.environ.get('INSPECTION_ADVISORY_FILE', DEFAULT_ADVISORY_FILE))
    opener = build_opener(NoRedirect())
    def api(path, data=None):
        body = None if data is None else json.dumps(data).encode()
        request = Request(gateway + path, data=body, headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
        with opener.open(request, timeout=8) as response:
            return json.loads(bounded(response, 8 * 1024 * 1024))
    path = '/v1/assets/inspections'
    started = time.monotonic()
    api(path, {'action':'tick'})
    totals = {'submitted':0, 'gaps':0, 'conflicts':0}
    for item in api(path+'?view=work')['work']:
        if time.monotonic()-started >= 90:
            break
        try:
            folder = root.joinpath(*asset_parts(item))
            if item['check_id'] == 'M2':
                evidence = machine_metrics(api,item)
            elif item['check_id'] in ('V1','V2','V3'):
                evidence = software(folder, advisory_file, item)
            else:
                evidence = receipt(folder,item)
            if not item['opened'] <= epoch(evidence['observed_at']) <= time.time():
                raise ValueError('sample outside frozen window')
        except HTTPError as exc:
            if exc.code not in (403,404,503):
                raise
            evidence = {'source':'gateway-monitor-exporter','observed_at':timestamp(),'gap':f'monitor_http_{exc.code}'}
        except (FileNotFoundError, PermissionError, KeyError, ValueError, TypeError, IndexError) as exc:
            # Store a fixed reason only. Parser errors can contain sensitive source values.
            gap = {FileNotFoundError: 'source_missing', PermissionError: 'source_unreadable'}.get(type(exc), 'incomplete_or_stale_source')
            evidence = {'source':'inspection-runner','observed_at':timestamp(),'gap':gap}
        try:
            reply=api(path, {'action':'evidence','item_id':item['item_id'],'generation':item['generation'],'evidence':evidence})
        except HTTPError as exc:
            if exc.code not in (403,404,409):
                raise
            totals['conflicts'] += 1
            continue
        totals['submitted'] += 1
        totals['gaps'] += int(reply['status'] == 'insufficient')
    print(json.dumps(totals))


if __name__ == '__main__':
    try:
        parser = argparse.ArgumentParser(description=__doc__)
        sbom = parser.add_subparsers(dest='command').add_parser('dpkg-sbom', help='publish a local installed-package SBOM')
        for arg in ('cluster', 'instance', 'asset-id'):
            sbom.add_argument('--' + arg, required=True)
        sbom.add_argument('--root', type=Path, required=True, help='existing evidence root on the observed host')
        args = parser.parse_args()
        if args.command == 'dpkg-sbom':
            subject = {k: getattr(args, k) for k in ('cluster', 'instance', 'asset_id')}
            print(json.dumps(collect_dpkg(args.root, subject)))
        else:
            run()
    except Exception as exc:
        print('inspection runner failed:', type(exc).__name__, getattr(exc,'code',''), file=sys.stderr)
        sys.exit(1)
