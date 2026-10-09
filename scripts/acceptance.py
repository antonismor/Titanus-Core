#!/usr/bin/env python3
"""Bounded CI or independently provisioned VM acceptance; see docs/ACCEPTANCE.md."""
import argparse
import concurrent.futures
import datetime
import hashlib
import json
import os
import pathlib
import platform
import re
import signal
import ssl
import stat
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
LIMIT = 2 << 20


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def save(path, value):
    data = (json.dumps(value, sort_keys=True, indent=2) + '\n').encode()
    with open(path, 'xb') as stream:
        os.chmod(path, 0o600)
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())


def private(path):
    path = pathlib.Path(path)
    st = path.lstat()
    if not stat.S_ISREG(st.st_mode) or st.st_uid != os.geteuid() or st.st_mode & 0o077:
        raise ValueError('private regular owner-only file required')
    return path


def command(argv, timeout, env=None, output=None):
    # A separate process group prevents timed-out hooks leaving command children.
    p = subprocess.Popen(argv, cwd=ROOT, env=env, start_new_session=True,
                         stdout=output or subprocess.DEVNULL, stderr=subprocess.STDOUT)
    try:
        return p.wait(timeout=timeout)
    except BaseException:
        os.killpg(p.pid, signal.SIGTERM)
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(p.pid, signal.SIGKILL)
            p.wait()
        raise


def host_facts(args):
    build = json.loads(subprocess.check_output([str(pathlib.Path(args.binary).resolve()), 'version', '--json'], timeout=10))
    memory = next(int(line.split()[1]) * 1024 for line in pathlib.Path('/proc/meminfo').read_text().splitlines() if line.startswith('MemTotal:'))
    model = next((line.split(':', 1)[1].strip() for line in pathlib.Path('/proc/cpuinfo').read_text().splitlines() if line.startswith(('model name', 'Hardware'))), 'unreported')
    facts = {'format': 'titanus-host-facts/v1', 'node_id': args.node_id, 'collected_at': utc(),
             'build': build, 'kernel': platform.release(), 'machine': platform.machine(),
             'boot_id_sha256': digest(pathlib.Path('/proc/sys/kernel/random/boot_id').read_bytes()),
             'cpu_count': os.cpu_count(), 'cpu_model': model, 'memory_bytes': memory,
             'cgroup_v2': pathlib.Path('/sys/fs/cgroup/cgroup.controllers').is_file()}
    save(args.output, facts)


def ci(args, out, report):
    if platform.system() != 'Linux' or os.geteuid() != 0:
        raise ValueError('CI native profile requires root on disposable Linux')
    sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT).decode().strip()
    if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT):
        raise ValueError('CI requires a clean committed checkout')
    builds = []
    for binary in ('titanus', 'titanusd', 'titanus-agent', 'titanus-init'):
        argv = [str(ROOT / 'bin' / binary), 'version', '--json'] if binary == 'titanus' else [str(ROOT / 'bin' / binary), '--version-json']
        builds.append(json.loads(subprocess.check_output(argv, timeout=10)))
    if any(b != builds[0] or b['revision'] != sha for b in builds):
        raise ValueError('all four binaries must match exact checkout revision')
    if not pathlib.Path('/sys/fs/cgroup/cgroup.controllers').is_file() or not pathlib.Path('/tmp/titanus-rootfs/bin/busybox').is_file():
        raise ValueError('prepared native Source and cgroups v2 required')
    report.update(revision=sha, build=builds[0], topology={'voters': 3, 'kernels': 1, 'transport': 'TLS loopback'},
                  kernel=platform.release(), cpu_count=os.cpu_count(), duration_requested_seconds=args.seconds, cycles=args.cycles)
    env = os.environ.copy()
    env.update(TITANUS_ACCEPTANCE_TEST='1', TITANUS_ACCEPTANCE_NATIVE_TEST='1',
               TITANUS_ACCEPTANCE_SECONDS=str(args.seconds), TITANUS_ACCEPTANCE_CYCLES=str(args.cycles),
               TITANUS_ACCEPTANCE_REVISION=sha, TITANUS_INIT_BINARY=str(ROOT / 'bin/titanus-init'))
    raw = out / 'go-test.ndjson'
    with raw.open('xb') as stream:
        code = command(['go', 'test', '-json', './internal/consensus', './internal/unitruntime',
                        '-run', '^Test(AcceptanceSustainedQuorum|NativeAcceptanceLifecycle)$',
                        '-count=1', '-timeout', '240s'], args.seconds + 180, env, stream)
    if raw.stat().st_size > 32 << 20:
        raise ValueError('CI raw evidence exceeds 32-MiB bound')
    measurements, passed = [], set()
    for line in raw.read_text().splitlines():
        event = json.loads(line)
        if event.get('Action') == 'pass' and event.get('Test'):
            passed.add(event['Test'])
        marker = 'TITANUS_ACCEPTANCE_EVIDENCE '
        if marker in event.get('Output', ''):
            measurements.append(json.loads(event['Output'].split(marker, 1)[1]))
    report.update(raw_sha256=digest(raw.read_bytes()), measurements=measurements,
                  tests_passed=sorted(passed), return_code=code)
    if code or passed != {'TestAcceptanceSustainedQuorum', 'TestNativeAcceptanceLifecycle'} or len(measurements) != 2:
        raise ValueError('required native tests/evidence missing or failed; inspect raw evidence')
    report['cleanup_verified'] = all(m.get('units_and_cgroups_cleaned', True) for m in measurements)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def origin(url, secure=True):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme not in (('https',) if secure else ('http', 'https')) or not parsed.hostname or parsed.username or parsed.password or parsed.fragment or parsed.query:
        raise ValueError('explicit credential-free HTTP(S) URL required')
    if secure and parsed.path not in ('', '/'):
        raise ValueError('API must be an HTTPS origin')
    return url.rstrip('/')


def get(opener, url, timeout):
    begin = time.monotonic()
    try:
        try:
            response = opener.open(urllib.request.Request(url, method='GET'), timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read(LIMIT + 1)
            if len(data) > LIMIT:
                raise ValueError('response exceeds 2-MiB bound')
            return response.code, data, time.monotonic() - begin
    except (OSError, ValueError, urllib.error.URLError):
        return 0, b'', time.monotonic() - begin


def json_get(opener, url, timeout):
    status, raw, elapsed = get(opener, url, timeout)
    try:
        value = json.loads(raw)
    except (ValueError, UnicodeError):
        value = {}
    return status, value, elapsed


def vm(args, out, report):
    inventory_path = private(args.inventory)
    raw_inventory = inventory_path.read_bytes()
    inv = json.loads(raw_inventory)
    if inv.get('format') != 'titanus-acceptance-inventory/v1' or not re.fullmatch('[0-9a-f]{40}', inv.get('revision', '')) or not inv.get('realm'):
        raise ValueError('invalid inventory/revision/Realm')
    nodes = inv['nodes']
    ids = [n['id'] for n in nodes]
    if not 5 <= len(nodes) <= 8 or len(set(ids)) != len(ids) or sum(n['role'] == 'controller' for n in nodes) != 3 or sum(n['role'] == 'worker' for n in nodes) < 2:
        raise ValueError('VM profile requires exactly 3 controllers and >=2 workers, at most 8 API hosts')
    base = inventory_path.resolve().parent
    resolve = lambda p: (base / p).resolve()
    context = ssl.create_default_context(cafile=str(resolve(inv['ca'])))
    context.load_cert_chain(str(resolve(inv['certificate'])), str(private(resolve(inv['key']))))
    opener = urllib.request.build_opener(NoRedirect(), urllib.request.HTTPSHandler(context=context))
    public = urllib.request.build_opener(NoRedirect())
    canary = origin(inv['canary']['url'], secure=False)
    expected_hash = inv['canary']['sha256']
    if not re.fullmatch('[0-9a-f]{64}', expected_hash):
        raise ValueError('exact canary response SHA256 required')
    facts = []
    for node in nodes:
        node['api'] = origin(node['api'])
        fact = json.loads(resolve(node['facts']).read_text())
        if fact.get('format') != 'titanus-host-facts/v1' or fact['node_id'] != node['id'] or fact['build']['revision'] != inv['revision'] or fact['build']['os'] != 'linux' or not fact['cgroup_v2']:
            raise ValueError('host facts do not match the expected native revision/node')
        facts.append(fact)
    if len({f['boot_id_sha256'] for f in facts}) != len(nodes):
        raise ValueError('distinct boot identities required; shared-kernel profile is CI only')
    report.update(revision=inv['revision'], inventory_sha256=digest(raw_inventory), topology=facts,
                  duration_requested_seconds=args.seconds, requests_per_second=args.rate,
                  integrity_sha256=expected_hash, host_facts_provenance='operator-collected per-host files; API build cross-checked live')
    fault = inv.get('fault')
    if fault:
        if not args.allow_faults:
            raise ValueError('fault commands require --allow-faults on the dedicated lab')
        for field in ('apply', 'revert'):
            if not isinstance(fault[field], list) or not fault[field] or not all(isinstance(a, str) and a for a in fault[field]):
                raise ValueError('fault hooks must be explicit nonempty argv arrays')
        if not set(fault['expected_unreachable']) <= set(ids) or not fault['expected_unreachable']:
            raise ValueError('fault must name the API hosts expected unreachable')
        if not 5 <= fault['after_seconds'] < args.seconds - 15 or not 5 <= fault['duration_seconds'] <= args.seconds - fault['after_seconds'] - 5:
            raise ValueError('fault timing must leave bounded baseline/recovery periods')
        report['fault'] = {'label': fault['label'], 'apply_argv_sha256': digest(json.dumps(fault['apply']).encode()),
                           'revert_argv_sha256': digest(json.dumps(fault['revert']).encode()), 'expected_unreachable': fault['expected_unreachable']}
    stop = threading.Event()
    lock = threading.Lock()
    counts = {'requests': 0, 'success': 0, 'http_errors': 0, 'integrity_failures': 0}
    latencies, samples, seen_fault = [], [], set()
    started = time.monotonic()

    def probe():
        sample = {'time': utc(), 'elapsed_seconds': time.monotonic() - started, 'nodes': []}
        def probe_node(node):
            code, info, _ = json_get(opener, node['api'] + '/v1/compatibility', args.timeout)
            build = info.get('capabilities', {}).get('build', {})
            okay = code == 200 and info.get('node_id') == node['id'] and info.get('realm') == inv['realm'] and build.get('revision') == inv['revision'] and build.get('arch') == facts[ids.index(node['id'])]['build']['arch']
            if code == 200 and not okay:
                raise ValueError('live API identity/revision mismatch')
            entry = {'id': node['id'], 'http_status': code, 'identity_verified': okay, 'architecture': build.get('arch')}
            if okay:
                dcode, diagnostics, _ = json_get(opener, node['api'] + '/v1/diagnostics', args.timeout)
                entry.update(diagnostics_status=dcode, realm_revision=diagnostics.get('realm_revision'),
                             incomplete=diagnostics.get('incomplete', ['unavailable']), ready_units=diagnostics.get('ready_units'),
                             observation_failures=diagnostics.get('observation_failures'), log_failures=diagnostics.get('log_sink_failures'))
                if node['role'] == 'controller':
                    _, consensus, _ = json_get(opener, node['api'] + '/v1/realm/consensus', args.timeout)
                    if consensus.get('state') == 'Leader':
                        acode, alerts, _ = json_get(opener, node['api'] + '/v1/realm/alerts', args.timeout)
                        entry['collection'] = {'http_status': acode, 'expected': alerts.get('expected'), 'collected': alerts.get('collected'),
                                               'alert_codes': [a.get('code') for a in alerts.get('alerts', [])][:256]}
            return entry
        with concurrent.futures.ThreadPoolExecutor(max_workers=len(nodes)) as probes:
            sample['nodes'] = list(probes.map(probe_node, nodes))
        return sample

    def complete(sample):
        return (all(n['identity_verified'] and n.get('diagnostics_status') == 200 and not n.get('incomplete') and n.get('log_failures') == 0 and n.get('observation_failures') == 0 for n in sample['nodes'])
                and any(n.get('collection', {}).get('http_status') == 200 and n['collection'].get('expected') == len(nodes) and n['collection'].get('collected') == len(nodes) for n in sample['nodes']))

    baseline = probe()
    samples.append(baseline)
    code, body, _ = get(public, canary, args.timeout)
    if not complete(baseline) or code != 200 or digest(body) != expected_hash:
        raise ValueError('baseline identity, complete diagnostics/collection and canary integrity required')

    def load(worker):
        next_request = started + worker / args.rate
        while not stop.is_set() and time.monotonic() - started < args.seconds:
            stop.wait(max(0, next_request - time.monotonic()))
            if stop.is_set():
                return
            code, body, elapsed = get(public, canary, args.timeout)
            with lock:
                counts['requests'] += 1
                latencies.append(elapsed)
                if code == 200 and digest(body) == expected_hash:
                    counts['success'] += 1
                elif code == 200:
                    counts['integrity_failures'] += 1
                else:
                    counts['http_errors'] += 1
            next_request = max(next_request + args.workers / args.rate, time.monotonic())

    attempted, reverted = False, False
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=args.workers)
    workers = [pool.submit(load, i) for i in range(args.workers)]
    try:
        while time.monotonic() - started < args.seconds:
            elapsed = time.monotonic() - started
            if fault and not attempted and elapsed >= fault['after_seconds']:
                attempted = True
                report['fault']['apply_at'] = utc()
                report['fault']['apply_return_code'] = command(fault['apply'], 30)
                if report['fault']['apply_return_code']:
                    raise ValueError('fault apply failed; revert still attempted')
            if fault and attempted and not reverted and elapsed >= fault['after_seconds'] + fault['duration_seconds']:
                report['fault']['revert_return_code'] = command(fault['revert'], 30)
                reverted = report['fault']['revert_return_code'] == 0
                report['fault']['revert_at'] = utc()
                if not reverted:
                    raise ValueError('fault revert failed')
            sample = probe()
            samples.append(sample)
            if fault and attempted and not reverted:
                seen_fault.update(n['id'] for n in sample['nodes'] if n['http_status'] == 0)
            stop.wait(5)
    finally:
        stop.set()
        pool.shutdown(wait=True)
        if fault and attempted and not reverted:
            try:
                report['fault']['revert_return_code'] = command(fault['revert'], 30)
                reverted = report['fault']['revert_return_code'] == 0
            except BaseException:
                report['fault']['revert_return_code'] = 'interrupted_or_timed_out'
        report['cleanup_verified'] = not attempted or reverted
        report['requests'] = counts
        report['observed_unreachable'] = sorted(seen_fault)
        for future in workers:
            if future.exception():
                report['load_worker_failed'] = True
        if latencies:
            ordered = sorted(latencies)
            report['latency_seconds'] = {'p50': ordered[len(ordered)//2], 'p95': ordered[(len(ordered)-1)*95//100], 'max': ordered[-1]}
        save(out / 'samples.json', samples)
        report['samples_sha256'] = digest((out / 'samples.json').read_bytes())
    recover = time.monotonic()
    while time.monotonic() - recover < args.recovery_seconds:
        final = probe()
        code, body, _ = get(public, canary, args.timeout)
        if complete(final) and code == 200 and digest(body) == expected_hash:
            save(out / 'recovered.json', final)
            report['recovery_seconds'] = time.monotonic() - recover
            break
        time.sleep(5)
    else:
        raise ValueError('bounded recovery did not restore all identities/diagnostics/coverage/integrity')
    if fault and not set(fault['expected_unreachable']) <= seen_fault:
        raise ValueError('configured fault not observed at named hosts')
    if not report['cleanup_verified'] or report.get('load_worker_failed') or counts['integrity_failures'] or counts['success'] == 0:
        raise ValueError('cleanup or workload integrity failed')
    if counts['http_errors'] / counts['requests'] > args.max_error_fraction or report['latency_seconds']['p95'] > args.max_p95_seconds:
        raise ValueError('declared availability/latency threshold exceeded')
    report['thresholds'] = {'max_error_fraction': args.max_error_fraction, 'max_p95_seconds': args.max_p95_seconds}
    report['scope'] = 'observed HTTP load, identity, diagnostics, coverage and configured fault; storage/task/power acceptance separate'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='profile', required=True)
    facts = sub.add_parser('host-facts')
    facts.add_argument('--node-id', required=True)
    facts.add_argument('--binary', default='/usr/local/lib/titanus/current/bin/titanus')
    facts.add_argument('--output', required=True)
    native = sub.add_parser('ci')
    native.add_argument('--seconds', type=int, choices=range(15, 121), default=30)
    native.add_argument('--cycles', type=int, choices=range(3, 33), default=10)
    native.add_argument('--output', required=True)
    lab = sub.add_parser('vm')
    lab.add_argument('--inventory', required=True)
    lab.add_argument('--output', required=True)
    lab.add_argument('--seconds', type=int, choices=range(60, 3601), default=600)
    lab.add_argument('--rate', type=int, choices=range(1, 21), default=5)
    lab.add_argument('--workers', type=int, choices=range(1, 9), default=2)
    lab.add_argument('--timeout', type=float, default=3)
    lab.add_argument('--recovery-seconds', type=int, choices=range(15, 121), default=90)
    lab.add_argument('--max-error-fraction', type=float, default=0)
    lab.add_argument('--max-p95-seconds', type=float, default=2)
    lab.add_argument('--allow-faults', action='store_true')
    args = parser.parse_args()
    os.umask(0o077)
    if args.profile == 'host-facts':
        host_facts(args)
        return
    if args.profile == 'vm' and (not 0.1 <= args.timeout <= 5 or not 0 <= args.max_error_fraction <= 1 or not 0.001 <= args.max_p95_seconds <= 60):
        parser.error('timeout/threshold outside documented bounds')
    out = pathlib.Path(args.output).resolve()
    out.mkdir(mode=0o700)  # Refuse replacing previous evidence.
    report = {'format': 'titanus-acceptance/v1', 'profile': args.profile, 'started_at': utc(),
              'independent_host_acceptance': False, 'result': 'failed', 'cleanup_verified': False}
    begin = time.monotonic()
    def interrupted(signum, frame):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupted)
    try:
        (ci if args.profile == 'ci' else vm)(args, out, report)
        report['result'] = 'passed'
    except (Exception, KeyboardInterrupt) as error:
        # Exception classes only: responses/commands may contain private values.
        report['failure_class'] = type(error).__name__
        print('Acceptance failed (' + type(error).__name__ + '); inspect private evidence and prerequisites.')
    finally:
        report.update(finished_at=utc(), elapsed_seconds=time.monotonic() - begin)
        save(out / 'summary.json', report)
    print(json.dumps({'result': report['result'], 'summary': str(out / 'summary.json')}))
    raise SystemExit(0 if report['result'] == 'passed' else 1)


if __name__ == '__main__':
    main()
