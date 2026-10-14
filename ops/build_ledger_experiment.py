"""Build isolated Mac measurements with a timing-only Go source overlay."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess

REPO = Path(__file__).resolve().parents[1]

def instrument(source):
    changes = [
        ('\tselect {\n\tcase s.mu <- struct{}{}:\n', '\tvalidationMutexStart := time.Now()\n\tselect {\n\tcase s.mu <- struct{}{}:\n\t\tvalidationObserve(ctx, "mutex", validationMutexStart)\n'),
        ('\tconn, err := s.db.Conn(ctx)\n', '\tvalidationConnectionStart := time.Now()\n\tconn, err := s.db.Conn(ctx)\n\tvalidationObserve(ctx, "connection_pool", validationConnectionStart)\n'),
        ('\t\t_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")\n', '\t\tvalidationDatabaseStart := time.Now()\n\t\t_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")\n\t\tvalidationObserve(ctx, "sqlite_begin", validationDatabaseStart)\n'),
    ]
    for before, after in changes:
        if source.count(before) != 1:
            raise ValueError('Source changed; review timing overlay before use.')
        source = source.replace(before, after)
    return source + '\nfunc init() { ledgerTimingOverlay = true }\n'

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', required=True)
    parser.add_argument('--output-dir', type=Path, required=True)
    args = parser.parse_args()
    output = args.output_dir.resolve()
    output.mkdir(parents=True, exist_ok=False)
    original = REPO / 'actionhost/files/store.go'
    raw = original.read_bytes()
    source = raw.decode().replace('\r\n', '\n')
    patched = instrument(source)
    # The companion test verifies that removing timing statements restores exact source.
    assert patched.count('validationObserve(') == 3
    overlay_source = output / 'store.go'
    overlay_source.write_text(patched, encoding='utf-8', newline='\n')
    overlay = output / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(original): str(overlay_source)}}), encoding='utf-8')
    binary = output / 'ledger-experiment.test'
    env = dict(os.environ, GOOS='darwin', GOARCH='arm64', CGO_ENABLED='0')
    command = [args.go, 'test', '-c', '-overlay', str(overlay), '-o', str(binary), './actionhost/files']
    run = subprocess.run(command, cwd=REPO, env=env, capture_output=True, text=True)
    (output / 'build.log').write_text(run.stdout + run.stderr, encoding='utf-8')
    record = {'head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=REPO).decode().strip(),
              'source_sha256': hashlib.sha256(raw).hexdigest(),
              'overlay_sha256': hashlib.sha256(overlay_source.read_bytes()).hexdigest(),
              'harness_sha256': hashlib.sha256((REPO / 'actionhost/files/validation_experiment_test.go').read_bytes()).hexdigest(),
              'package_sources_sha256': {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted((REPO / 'actionhost/files').glob('*.go'))},
              'go_version': subprocess.check_output([args.go, 'version'], env=env).decode().strip(),
              'returncode': run.returncode, 'target': 'darwin/arm64',
              'sqlite_busy_timeout_ms': 50,
              'measurement_note': 'sqlite_begin includes SQL execution plus DB lock waiting; connection_pool and mutex measured separately; 50ms retry backoff remains in total.'}
    if run.returncode == 0:
        record['binary_sha256'] = hashlib.sha256(binary.read_bytes()).hexdigest()
    (output / 'build.json').write_text(json.dumps(record, indent=2), encoding='utf-8')
    print(json.dumps(record))
    raise SystemExit(run.returncode)

if __name__ == '__main__':
    main()
