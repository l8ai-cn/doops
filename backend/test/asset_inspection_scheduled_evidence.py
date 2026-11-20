"""Verify a downloaded Gateway weekly report locally. Usage: python3 FILE.py REPORT.json"""
import argparse
import hashlib
import json
import re
from datetime import datetime
from pathlib import Path


def verify(raw):
    report = json.loads(raw)
    run = report['run']
    if run['trigger_kind'] != 'scheduled':
        raise ValueError('expected scheduled run; found ' + run['trigger_kind'])
    if run['frozen'] < run['due'] or not run['executor']:
        raise ValueError('invalid scheduled run time or executor')
    history = {(h['item_id'], h['generation']): h for h in report['history']}
    if len(history) != len(report['history']):
        raise ValueError('duplicate receipt for the same attempt')
    ids = set()
    samples = []
    for item in report['items']:
        key = item['item_id']
        if key in ids:
            raise ValueError('duplicate frozen item')
        ids.add(key)
        if item['status'] not in ('pass', 'finding'):
            continue
        receipt = history[(key, item['generation'])]
        ev = receipt['evidence']
        observed = datetime.fromisoformat(ev['observed_at'].replace('Z', '+00:00'))
        if (receipt['status'] != item['status'] or receipt['actor'] != run['executor'] or
            not ev['source'] or ev['source'] != receipt['source'] or ev.get('gap') or
            receipt['evidence_category'] not in ('collector_summary','collector_gateway_metric','offline_advisory_comparison') or
            not re.fullmatch(r'[0-9a-f]{64}', ev['sha256']) or not re.fullmatch(r'[0-9a-f]{64}', receipt['digest']) or
            observed.tzinfo is None or not run['frozen'] <= item['opened'] <= int(observed.timestamp()) <= receipt['received'] < item['deadline']):
            raise ValueError(f'inconsistent source, identity or collection window for item {key}')
        samples.append({'item_id':key, 'asset_id':item['asset_id'], 'target':[item['cluster'],item['instance']],
                        'check_id':item['check_id'], 'generation':item['generation'], 'status':item['status'],
                        'source':ev['source'], 'observed_at':ev['observed_at'], 'source_sha256':ev['sha256']})
    # A manually requested makeup of an otherwise missed scheduled run is not
    # evidence that the initial automatic collection actually returned a sample.
    first = [h for h in report['history'] if h['item_id'] in ids and h['generation']==1 and
             h['status'] in ('pass','finding') and h['actor']==run['executor'] and isinstance(h['evidence'],dict) and
             h['evidence'].get('source') and re.fullmatch(r'[0-9a-f]{64}',h['evidence'].get('sha256',''))]
    if not first:
        raise ValueError('no effective receipt from the original scheduled attempt')
    return {'report_sha256':hashlib.sha256(raw).hexdigest(), 'run_id':run['run_id'], 'trigger_kind':'scheduled',
            'executor':run['executor'], 'due':run['due'], 'frozen':run['frozen'], 'original_observed_items':len(first),
            'planned_items':len(ids), 'current_observed_items':len(samples), 'current_samples':samples}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('report',type=Path)
    args = parser.parse_args()
    try:
        print(json.dumps(verify(args.report.read_bytes()), ensure_ascii=False, indent=2))
    except (OSError, ValueError, TypeError, KeyError, OverflowError) as error:
        parser.exit(1, f'Weekly report verification failed: {error}\n')
