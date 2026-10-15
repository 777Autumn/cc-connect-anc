"""CI-only Claude/Codex protocol substitute; never used for live delivery."""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import zipfile


def main():
    config = json.loads(Path(sys.argv[1]).read_text())
    assert os.geteuid() == config['ModelUID'] != config['GatewayUID']
    assert Path.cwd() == Path(config['EmployeeWork'])
    if sys.platform == 'linux':
        assert 'CapEff:\t0000000000000000' in Path('/proc/self/status').read_text()
    root = os.environ['CC_FILE_WORK_ROOT']
    codex = 'app-server' in sys.argv
    if codex:
        assert f'sandbox_workspace_write.writable_roots=[{json.dumps(root)}]' in sys.argv
    else:
        assert sys.argv[sys.argv.index('--add-dir') + 1] == root
    assert os.environ['CC_FILE_TRANSPORT'] == 'native'
    # The real Claude sandbox supplies this proxy. Here only its HTTP boundary
    # is replaced, not the production file client or gateway tool handler.
    os.environ['HTTP_PROXY'] = config['Proxy']
    os.environ.pop('http_proxy', None)
    for path in config['PrivateFiles']:
        try:
            Path(path).read_bytes()
        except PermissionError:
            pass
        else:
            raise AssertionError('host-private file readable')

    def tool(command, value):
        result = subprocess.run([sys.executable, '-B', config['Client'], command],
            input=json.dumps(value), capture_output=True, text=True, timeout=10)
        assert result.returncode == 0, result.stdout
        return json.loads(result.stdout)

    for line in sys.stdin:
        message = json.loads(line)
        if codex:
            method = message.get('method')
            if 'id' not in message:
                continue
            result = {}
            if method in ('thread/start', 'thread/resume'):
                result = {'thread': {'id': 'fictional-native-session'}, 'cwd': str(Path.cwd()),
                          'model': 'synthetic-codex', 'reasoningEffort': 'medium'}
            elif method == 'turn/start':
                result = {'turn': {'id': 'fictional-turn', 'status': 'inProgress'}}
            print(json.dumps({'id': message['id'], 'result': result}), flush=True)
            if method != 'turn/start':
                continue
        elif message.get('type') != 'user':
            continue
        for extension in ('xlsx', 'docx'):
            work = tool('work-context', {})
            original = next(i for i in work['inputs'] if i['name'].endswith('.' + extension))
            data = Path(original['path']).read_bytes()
            assert hashlib.sha256(data).hexdigest() == original['sha256']
            try:
                with open(original['path'], 'ab'):
                    pass
            except PermissionError:
                pass
            else:
                raise AssertionError('original writable')
            previous = [a for a in work['artifacts'] if a['path'].endswith('.' + extension)]
            if previous:
                data = (Path(work['output_dir']) / previous[-1]['path']).read_bytes()
                assert hashlib.sha256(data).hexdigest() == previous[-1]['sha256']
            output = io.BytesIO()
            with zipfile.ZipFile(io.BytesIO(data)) as source, zipfile.ZipFile(output, 'w') as target:
                for name in source.namelist():
                    content = source.read(name)
                    if name.endswith('.xml'):
                        content += b'\n<!-- fictional revision -->'
                    target.writestr(name, content)
            name = f'revision-{work["latest_version"] + 1}.{extension}'
            path = Path(work['output_dir']) / name
            with path.open('xb') as stream:
                stream.write(output.getvalue())
            path.chmod(0o600)
            read = tool('file-read', {'work_id':work['work_id'], 'path':name,
                'sha256':hashlib.sha256(path.read_bytes()).hexdigest()})
            assert read['status'] == 'content_read' and read['content']['parts']
            receipt = tool('file-deliver', {'work_id':work['work_id'], 'path':name,
                'sha256':hashlib.sha256(path.read_bytes()).hexdigest(), 'expected_version':work['latest_version']})
            assert receipt['status'] == 'accepted' and receipt['message_receipt']
        if codex:
            for method, params in (
                ('item/completed', {'threadId': 'fictional-native-session', 'turnId': 'fictional-turn',
                                    'item': {'type': 'agentMessage', 'id': 'result', 'text': 'NATIVE_FIXTURE_DONE'}}),
                ('turn/completed', {'threadId': 'fictional-native-session',
                                    'turn': {'id': 'fictional-turn', 'status': 'completed'}})):
                print(json.dumps({'method': method, 'params': params}), flush=True)
        else:
            print(json.dumps({'type':'result', 'subtype':'success', 'result':'NATIVE_FIXTURE_DONE',
                              'session_id':'fictional-native-session'}), flush=True)


if __name__ == '__main__':
    main()
