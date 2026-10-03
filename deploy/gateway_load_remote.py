"""Server-local bounded load runner, paired with read-only process sampling."""
import argparse
import hashlib
import json
from pathlib import Path
import signal
import subprocess
import sys
import time


def runtime():
    pid = subprocess.check_output(['systemctl', 'show', 'codex-agent.service', '--value', '--property=MainPID'], text=True).strip()
    if not pid.isdigit() or int(pid) <= 0:
        raise RuntimeError('gateway_not_running')
    proc = Path('/proc') / pid
    return int(pid), hashlib.sha256((proc / 'exe').read_bytes()).hexdigest()


def reap(process):
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def execute(folder, args):
    folder = Path(folder)
    stop = folder / 'monitor.stop'
    if stop.exists() or (folder / 'runner.json').exists():
        raise RuntimeError('run_directory_already_used')
    report = {'scope': args.scope, 'status_scope': 'orchestration_only_not_capacity_acceptance',
              'started_at': time.time(), 'status': 'fail',
              'load_exit_code': None, 'monitor_exit_code': None, 'runtime_stable': False}
    load = monitor = None
    output = log = None
    try:
        pid, digest = runtime()
        report.update(pid_before=pid, binary_sha256_before=digest)
        (folder / 'metadata.json').write_text(json.dumps({'binary_sha256': digest}))
        output = (folder / 'monitor-output.txt').open('w')
        log = (folder / 'load-output.txt').open('w')
        monitor = subprocess.Popen([sys.executable, str(folder / 'monitor.py'), '--pid', str(pid),
                '--admin-base', 'http://127.0.0.1:18879', '--key-file', '/var/lib/codex-agent/client-key',
                '--interval', '.2', '--duration', '1700', '--stop-file', str(stop), '--output', str(folder / 'monitor')],
                stdout=output, stderr=subprocess.STDOUT)
        if monitor.poll() is not None:
            raise RuntimeError('monitor_exited_before_load')
        load = subprocess.Popen([sys.executable, str(folder / 'live_load.py'),
                    '--base-url', 'http://127.0.0.1:18879/v1', '--model', args.model,
                    '--key-file', '/var/lib/codex-agent-workbench-bridge/' + args.scope + '-key',
                    '--levels', args.levels, '--waves', str(args.waves), '--timeout', '120',
                    '--cooldown', '15', '--max-cooldown', '120', '--continue-after-429',
                    '--metadata-file', str(folder / 'metadata.json'), '--output', str(folder / 'load')],
                stdout=log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 1650
        while load.poll() is None:
            if monitor.poll() is not None:
                raise RuntimeError('monitor_exited_during_load')
            if time.monotonic() >= deadline:
                raise RuntimeError('load_deadline_exceeded')
            time.sleep(.1)
    except BaseException as error:
        allowed = {'monitor_exited_before_load', 'monitor_exited_during_load', 'load_deadline_exceeded'}
        report['error_code'] = str(error) if isinstance(error, RuntimeError) and str(error) in allowed else type(error).__name__
    finally:
        try:
            reap(load)
        except Exception:
            report['cleanup_error'] = 'load_process_cleanup_failed'
        try:
            stop.touch()
            if monitor is not None:
                try:
                    monitor.wait(timeout=25)
                except subprocess.TimeoutExpired:
                    report['monitor_forced_stop'] = True
                    reap(monitor)
        except Exception:
            report['cleanup_error'] = 'monitor_process_cleanup_failed'
        for handle in (output, log):
            if handle is not None:
                handle.close()
        report.update(load_exit_code=load.returncode if load is not None else None,
                      monitor_exit_code=monitor.returncode if monitor is not None else None)
        try:
            end_pid, end_digest = runtime()
            report.update(pid_after=end_pid, binary_sha256_after=end_digest,
                runtime_stable=end_pid == report.get('pid_before') and end_digest == report.get('binary_sha256_before'))
        except Exception:
            report['runtime_after_unavailable'] = True
        if (report['load_exit_code'] == 0 and report['monitor_exit_code'] == 0 and report['runtime_stable']
                and not any(key in report for key in ('error_code', 'cleanup_error', 'monitor_forced_stop'))):
            report['status'] = 'pass'
        report['finished_at'] = time.time()
        temporary = folder / 'runner.json.tmp'
        temporary.write_text(json.dumps(report, indent=2))
        temporary.replace(folder / 'runner.json')
    print(json.dumps(report))
    return 0 if report['status'] == 'pass' else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--scope', choices=['single', 'pool'], required=True)
    parser.add_argument('--model', default='gpt-5.6-luna')
    parser.add_argument('--levels', default='30,50,70,100')
    parser.add_argument('--waves', type=int, default=2)
    args = parser.parse_args()

    def interrupted(signum, frame):
        raise InterruptedError('runner_interrupted')

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    return execute(Path(__file__).resolve().parent, args)


if __name__ == '__main__':
    raise SystemExit(main())
