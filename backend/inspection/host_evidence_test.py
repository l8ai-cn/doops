import contextlib
import hashlib
import io
import json
import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import host_evidence as host
import runner


class HostEvidenceTest(unittest.TestCase):
    def setUp(self):
        scratch = tempfile.TemporaryDirectory()
        self.addCleanup(scratch.cleanup)
        self.root = Path(scratch.name)
        self.output = self.root / 'out'
        self.output.mkdir()
        self.subject = dict(asset_id='S1', cluster='dev', instance='local')
        self.folder = self.output / 'dev/local/S1'
        self.now = int(time.time())
        self.policy = {'M1': {'min_uptime_seconds': 60}, 'M3': {'denied_comm': ['sample-test']},
                       'M4': {'sysctl': {'kernel.kptr_restrict': 2}},
                       'M5': {'privileged_groups': ['sudo'], 'window_seconds': 30, 'max_gap_seconds': 10}}
        self.config = self.put('local.json', json.dumps(self.policy))
        self.stat = self.put('proc/stat', f'cpu 1 2 3 4\nbtime {self.now - 65}\n')
        self.put('proc/uptime', '65.25 44.00\n')
        self.mount = self.put('proc/self/mountinfo', '42 1 0:5 / /proc rw - proc proc rw\n')
        self.put('proc/1/comm', 'init\n')
        self.comm = self.put('proc/18/comm', 'worker\n')
        self.sysctl = self.put('proc/sys/kernel/kptr_restrict', '2\n')
        self.log = self.root / 'var/log/audit/audit.log'
        self.log.parent.mkdir(parents=True)
        self.audit()

    def put(self, name, value):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value)
        return path

    def audit(self, result='success', group='staff'):
        events = [(-45, 'SERVICE_START', ''), (-39, 'USER_AUTH', f"msg='op=PAM:authentication acct=private-user res={result}'"),
                  (-30, 'GRP_MGMT', f'''msg='op=add-user-to-group grp="{group}" res=success' '''),
                  (-22, 'USER_LOGIN', "msg='res=success'"), (-15, 'SERVICE_START', ''), (-8, 'SERVICE_START', '')]
        self.log.write_text(''.join(f'type={kind} msg=audit({self.now+t}.000:{i}): {body}\n'
                                    for i, (t, kind, body) in enumerate(events, 10)))

    def collect(self):
        return host.collect(self.subject, self.output, self.config, root=self.root, now=self.now)

    def read(self, check):
        return runner.receipt(self.folder, dict(self.subject, check_id=check))

    def test_green_and_findings_read_back_through_runner(self):
        self.assertEqual(self.collect(), {})
        for check in host.CHECKS:
            receipt = self.read(check)
            self.assertEqual(receipt['violations'], 0)
            self.assertGreater(receipt['samples'], 0)
            self.assertLessEqual(len(receipt['policy'].encode()), 160)
            self.assertEqual(receipt['sha256'], hashlib.sha256((self.folder / f'{check}.json').read_bytes()).hexdigest())
        self.assertEqual(runner.epoch(self.read('M5')['observed_at']), self.now - 10)
        self.policy['M1']['min_uptime_seconds'] = 90
        self.config.write_text(json.dumps(self.policy))
        self.comm.write_text('sample-test\n')
        self.sysctl.write_text('1\n')
        self.audit('failed', 'sudo')
        self.assertEqual(self.collect(), {})
        self.assertEqual([self.read(k)['violations'] for k in host.CHECKS], [1, 1, 1, 2])
        self.assertNotIn('private-user', str(self.read('M5')))
        self.assertNotIn('sample-test', str(self.read('M3')))

    def test_withdraw_all_stale_receipts_when_evidence_becomes_partial(self):
        self.assertEqual(self.collect(), {})
        self.stat.write_text('btime 0\n')
        self.comm.unlink()
        self.sysctl.unlink()
        self.log.write_text(self.log.read_text().replace(':12)', ':123)'))
        self.assertEqual(set(self.collect()), set(host.CHECKS))
        for check in host.CHECKS:
            with self.assertRaises(FileNotFoundError):
                self.read(check)

    def test_audit_boundaries_and_source_types(self):
        full = self.log.read_text().splitlines(keepends=True)
        cases = {'rotation': full[1:], 'old tail': full[:-1], 'quiet period': [s.replace(f'{self.now-30}.', f'{self.now-25}.') for s in full],
                 'no group records': [s.replace('GRP_MGMT', 'SERVICE_STOP') for s in full],
                 'no login records': [s.replace('USER_AUTH', 'SERVICE_STOP').replace('USER_LOGIN', 'SERVICE_START') for s in full],
                 'unknown result': [s.replace('res=success', 'res=unknown') for s in full],
                 'lost messages': [s.replace('type=USER_LOGIN', 'type=DAEMON_LOST') for s in full],
                 'loss at trailing boundary': full[:-1] + [full[-1].replace('SERVICE_START', 'DAEMON_LOST')],
                 'incomplete last line': full[:-1] + [full[-1].rstrip()],
                 'unreadable group': [s.replace('grp="staff"', 'gid=2000') for s in full]}
        for label, lines in cases.items():
            with self.subTest(label):
                self.log.write_text(''.join(lines))
                self.assertEqual(set(self.collect()), {'M5'})
                self.assertFalse((self.folder / 'M5.json').exists())

    def test_absent_policy_does_not_enable_checks_or_keep_old_pass(self):
        self.assertEqual(self.collect(), {})
        self.config.unlink()
        self.assertEqual(set(self.collect()), set(host.CHECKS))
        self.assertEqual(list(self.folder.iterdir()), [])
        self.config.write_text(json.dumps({'M4': {'sysctl': {'../private': 0}}, 'M3': {'denied_comm': []}}))
        self.assertEqual(set(self.collect()), set(host.CHECKS))

    def test_proc_visibility_and_boot_consistency(self):
        self.mount.write_text(self.mount.read_text().replace('proc proc rw', 'proc proc rw,hidepid=2'))
        self.stat.write_text(f'btime {self.now-6000}\n')
        self.assertEqual(set(self.collect()), {'M1', 'M3'})

    def test_write_failure_withdraws_previous_receipt(self):
        self.assertEqual(self.collect(), {})
        with patch.object(host.os, 'replace', side_effect=OSError('disk failure')):
            with self.assertRaises(OSError):
                self.collect()
        self.assertFalse((self.folder / 'M1.json').exists())
        self.assertEqual(sorted(p.name for p in self.folder.iterdir()), ['M3.json', 'M4.json', 'M5.json'])

    def test_policy_symlink_and_asset_traversal(self):
        self.assertEqual(self.collect(), {})
        policy = self.config.rename(self.root / 'other-policy')
        self.config.symlink_to(policy)
        self.assertEqual(set(self.collect()), set(host.CHECKS))
        self.subject['asset_id'] = '../elsewhere'
        with self.assertRaises(host.Gap):
            self.collect()
        self.assertFalse((self.output / 'dev/elsewhere').exists())

    def test_same_directory_replace(self):
        self.collect()
        original = os.replace
        def observe(src, dest):
            self.assertEqual(src.parent, dest.parent)
            self.assertIn('check_id', json.loads(src.read_text()))
            self.assertIn('check_id', json.loads(dest.read_text()))
            original(src, dest)
        with patch.object(host.os, 'replace', side_effect=observe) as replace:
            self.assertEqual(self.collect(), {})
            self.assertEqual(replace.call_count, 4)
        self.assertEqual(len(list(self.folder.iterdir())), 4)

    def test_unconfigured_sg_work_item_submits_runner_gap(self):
        self.assertEqual(self.collect(), {})
        token = self.put('test-token', 'fixture-token')
        work = [dict(self.subject, cluster='SG', item_id=i, generation=1, opened=self.now-120, check_id=k)
                for i, k in enumerate(host.CHECKS)]
        submissions = []
        class LocalAPI:
            def open(self, req, timeout):
                if req.data is None:
                    value = {'work': work}
                else:
                    body = json.loads(req.data)
                    if body['action'] == 'evidence':
                        submissions.append(body['evidence'])
                    value = {'status': 'insufficient'}
                return io.BytesIO(json.dumps(value).encode())
        env = {'INSPECTION_GATEWAY': 'http://localhost', 'INSPECTION_TOKEN_FILE': str(token), 'INSPECTION_EVIDENCE_ROOT': str(self.output)}
        with patch.dict(os.environ, env), patch.object(runner, 'build_opener', return_value=LocalAPI()), contextlib.redirect_stdout(io.StringIO()):
            runner.run()
        self.assertEqual([e['gap'] for e in submissions], ['source_missing'] * 4)
        self.assertTrue(all('violations' not in e for e in submissions))


if __name__ == '__main__':
    unittest.main()
