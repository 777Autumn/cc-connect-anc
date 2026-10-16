"""Offline file-client contract, using a fake Unix socket and real HTTP parser."""
import base64
import errno
import hashlib
import io
import json
import os
import socket
import stat
import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch

import file_tool


class Peer:
    def __init__(self, result, status=200):
        body = json.dumps(result).encode()
        self.response = (f"HTTP/1.1 {status} Fixture\r\nContent-Type: application/json\r\n"
                         f"Content-Length: {len(body)}\r\n\r\n").encode() + body
        self.sent = bytearray()
        self.address = None
        self.closed = False

    def settimeout(self, timeout):
        self.timeout = timeout

    def setsockopt(self, *_args):
        pass

    def connect(self, address):
        self.address = address

    def sendall(self, data):
        self.sent.extend(data)

    def makefile(self, *_args):
        return io.BytesIO(self.response)

    def close(self):
        self.closed = True


class FileClientTests(unittest.TestCase):
    def test_content_read_requires_actual_matching_content_response(self):
        value = {'work_id': 'fictional', 'path': 'render/final.docx', 'sha256': 'a' * 64}
        expected = {**value, 'status': 'content_read', 'content_sha256': 'b' * 64,
                    'read_version': 2, 'chunk': 0, 'chunks_total': 1, 'chunks_provided': 1,
                    'part_offset': 0, 'part_bytes': 26,
                    'content': {'format': 'ooxml_content_parts', 'parts': [{'name': 'word/document.xml', 'text': '<document>Final</document>'}]}}
        with patch.object(file_tool, 'prepare_delivery'):
            code, result, _ = self.invoke(['file-read'], json.dumps(value).encode(), Peer(expected))
            self.assertEqual((code, result), (0, expected))
            for bad in ({**expected, 'path': 'source.docx'}, {**expected, 'sha256': 'c' * 64},
                        {**expected, 'content': {}}, {**expected, 'work_id': 'other-work'}):
                code, result, _ = self.invoke(['file-read'], json.dumps(value).encode(), Peer(bad))
                self.assertEqual((code, result['code']), (1, 'invalid_response'))

    def test_content_read_forwards_only_one_bounded_segment_and_rejects_legacy_proof(self):
        value = {'work_id': 'fictional', 'path': 'final.docx', 'sha256': 'a' * 64, 'chunk': 1}
        expected = {**value, 'status': 'content_read_partial', 'content_sha256': 'b' * 64,
                    'read_version': 2, 'chunks_total': 3, 'chunks_provided': 2, 'next_chunk': 2,
                    'part_offset': 4, 'part_bytes': 12,
                    'content': {'format': 'ooxml_content_parts', 'parts': [{'name': 'word/document.xml', 'text': 'body'}]}}
        with patch.object(file_tool, 'prepare_delivery'):
            peer = Peer(expected)
            code, result, _ = self.invoke(['file-read'], json.dumps(value).encode(), peer)
            self.assertEqual((code, result), (0, expected))
            self.assertEqual(bytes(peer.sent).count(b'POST /tool HTTP/1.1'), 1)
            for bad in ({**expected, 'read_version': 0}, {**expected, 'chunk': 0},
                        {**expected, 'next_chunk': 1}, {**expected, 'chunks_provided': 1},
                        {**expected, 'status': 'content_read'},
                        {**expected, 'scope': 'x' * 8192}):
                code, result, _ = self.invoke(['file-read'], json.dumps(value).encode(), Peer(bad))
                self.assertEqual((code, result['code']), (1, 'invalid_response'))
            for chunk in (-1, True, 512, '1'):
                code, result, factory = self.invoke(['file-read'], json.dumps(dict(value, chunk=chunk)).encode(), Peer(expected))
                self.assertEqual((code, result['code']), (2, 'invalid_input'))
                factory.assert_not_called()

    def test_bounded_content_response_is_not_rejected_by_added_client_whitespace(self):
        value = {'work_id': 'fictional', 'path': 'seg/' * 900 + 'final.docx', 'sha256': 'a' * 64}
        expected = {**value, 'status': 'content_read', 'content_sha256': 'b' * 64,
                    'read_version': 2, 'chunk': 0, 'chunks_total': 1, 'chunks_provided': 1,
                    'part_offset': 0, 'part_bytes': 4096,
                    'content': {'format': 'ooxml_content_parts', 'parts': [{'name': 'word/document.xml', 'text': 'x' * 4096}]}}
        compact = lambda item: json.dumps(item, ensure_ascii=False, separators=(',', ':')).encode()
        expected['scope'] = ''
        expected['scope'] = 'x' * (8188 - len(compact(expected)))
        self.assertEqual(len(compact(expected)), 8188)
        self.assertGreater(len(json.dumps(expected).encode()), 8192)
        with patch.object(file_tool, 'prepare_delivery'):
            code, result, _ = self.invoke(['file-read'], json.dumps(value).encode(), Peer(expected))
        self.assertEqual((code, result), (0, expected))
        out = io.StringIO()
        file_tool.emit(out, result, 0)
        self.assertLessEqual(len(out.getvalue().encode()), 8192)

    @unittest.skipUnless(os.name == 'posix', 'POSIX ownership and no-follow publication')
    def test_only_selected_owned_output_is_published_and_links_digest_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            previous = os.getcwd()
            try:
                os.chdir(directory)
                Path('outputs/nested').mkdir(parents=True, mode=0o700)
                selected = Path('outputs/nested/result.docx')
                selected.write_bytes(b'fictional-docx-client-fixture')
                selected.chmod(0o600)
                untouched = Path('outputs/private.txt')
                untouched.write_bytes(b'private-draft')
                untouched.chmod(0o600)
                value = {'work_id':'fixture', 'path':'nested/result.docx',
                         'sha256':hashlib.sha256(selected.read_bytes()).hexdigest(), 'expected_version':0}
                accepted = {'delivery_id':'fixture-delivery', 'status':'accepted', 'message_receipt':'fixture-message'}
                stale = dict(value, sha256='0' * 64)
                peer = Peer(accepted)
                code, result, factory = self.invoke(['file-deliver'], json.dumps(stale).encode(), peer)
                self.assertEqual((code, result), (2, {
                    'status': 'blocked', 'code': 'file_access_not_prepared',
                    'stage': 'local_preflight', 'request_state': 'not_sent', 'reason': 'output_changed'}))
                factory.assert_not_called()
                self.assertEqual(bytes(peer.sent), b'')
                self.assertEqual(stat.S_IMODE(selected.stat().st_mode), 0o600)
                value['sha256'] = hashlib.sha256(selected.read_bytes()).hexdigest()
                peer = Peer(accepted)
                code, result, factory = self.invoke(['file-deliver'], json.dumps(value).encode(), peer)
                self.assertEqual((code,result), (0,accepted))
                self.assertEqual(factory.call_count, 1)
                self.assertEqual(bytes(peer.sent).count(b'POST /tool HTTP/1.1'), 1)
                self.assertEqual(stat.S_IMODE(selected.stat().st_mode), 0o640)
                self.assertEqual(stat.S_IMODE(selected.parent.stat().st_mode), 0o2750)
                self.assertEqual(stat.S_IMODE(untouched.stat().st_mode), 0o600)
                for kind in ('digest', 'missing', 'symlink', 'hardlink'):
                    with self.subTest(kind=kind):
                        bad = dict(value)
                        selected.chmod(0o600)
                        if kind == 'digest':
                            bad['sha256'] = '0' * 64
                        elif kind == 'missing':
                            bad['path'] = 'absent.docx'
                        else:
                            link = Path('outputs/' + kind)
                            if kind == 'symlink':
                                link.symlink_to('nested/result.docx')
                            else:
                                os.link(selected, link)
                            bad['path'] = kind
                        code, result, factory = self.invoke(['file-deliver'], json.dumps(bad).encode(), Peer(accepted))
                        self.assertEqual((code,result['code']), (2,'file_access_not_prepared'))
                        self.assertEqual(result['reason'], 'output_changed' if kind == 'digest' else 'invalid_output')
                        self.assertEqual(result['request_state'], 'not_sent')
                        factory.assert_not_called()
                        self.assertEqual(stat.S_IMODE(selected.stat().st_mode), 0o600)
            finally:
                os.chdir(previous)

    def test_local_preflight_reasons_never_connect_or_expose_exception_text(self):
        raw = json.dumps({'work_id': 'fixture', 'path': 'result.docx',
                          'sha256': '0' * 64, 'expected_version': 0}).encode()
        cases = [(PermissionError('private-path'), 'access_denied'),
                 (FileNotFoundError('private-path'), 'invalid_output'),
                 (NotADirectoryError('private-path'), 'invalid_output'),
                 (IsADirectoryError('private-path'), 'invalid_output'),
                 (OSError(errno.ELOOP, 'private-path'), 'invalid_output'),
                 (OSError(errno.ENAMETOOLONG, 'private-path'), 'invalid_output'),
                 (ValueError('private-path'), 'invalid_output'),
                 (OSError(errno.EIO, 'private-path'), 'preparation_failed'),
                 (AttributeError('private-path'), 'preparation_failed')]
        for error, reason in cases:
            with self.subTest(error=type(error).__name__, reason=reason):
                peer = Peer({})
                with patch.object(file_tool, 'prepare_delivery', side_effect=error):
                    code, result, factory = self.invoke(['file-deliver'], raw, peer)
                self.assertEqual((code, result), (2, {
                    'status': 'blocked', 'code': 'file_access_not_prepared',
                    'stage': 'local_preflight', 'request_state': 'not_sent', 'reason': reason}))
                factory.assert_not_called()
                self.assertIsNone(peer.address)
                self.assertEqual(bytes(peer.sent), b'')

    def test_delivery_host_results_and_post_request_uncertainty_never_become_not_sent(self):
        raw = json.dumps({'work_id': 'fixture', 'path': 'result.docx',
                          'sha256': '0' * 64, 'expected_version': 0}).encode()
        cases = [(Peer({'status': 'blocked', 'code': 'file_version_conflict'}, 400),
                  {'status': 'blocked', 'code': 'file_version_conflict'}, 1),
                 (Peer({'status': 'unavailable', 'code': 'file_outcome_unconfirmed'}, 503),
                  {'status': 'unavailable', 'code': 'file_outcome_unconfirmed'}, 1),
                 (Peer({'status': 'unknown', 'delivery_id': 'fixture-delivery'}),
                  {'status': 'unknown', 'delivery_id': 'fixture-delivery'}, 0),
                 (Peer({'status': 'accepted'}),
                  {'status': 'unavailable', 'code': 'invalid_response'}, 1)]
        timeout = Peer({})
        timeout_stream = io.BytesIO()
        timeout_stream.readline = lambda *_args: (_ for _ in ()).throw(TimeoutError('private-path'))
        timeout.makefile = lambda *_args: timeout_stream
        cases.append((timeout, {'status': 'unavailable', 'code': 'outcome_unconfirmed'}, 1))
        for peer, expected, exit_code in cases:
            with self.subTest(expected=expected):
                with patch.object(file_tool, 'prepare_delivery'):
                    code, result, factory = self.invoke(['file-deliver'], raw, peer)
                self.assertEqual((code, result), (exit_code, expected))
                self.assertEqual(factory.call_count, 1)
                self.assertEqual(bytes(peer.sent).count(b'POST /tool HTTP/1.1'), 1)
                self.assertTrue(peer.closed)

    def invoke(self, command, raw, peer, env=None):
        output = io.StringIO()
        environment = {"CC_FILE_ACTION_TOKEN": "fixture-token",
                       "CC_ACTION_TOOLS_SOCKET": file_tool.SOCKET}
        if env:
            environment.update(env)
        with (patch.dict(os.environ, environment, clear=True),
              patch.object(socket, "AF_UNIX", getattr(socket, "AF_UNIX", 99), create=True),
              patch.object(socket, "create_connection", return_value=peer),
              patch.object(socket, "socket", return_value=peer) as factory):
            code = file_tool.main(command, stdin=io.BytesIO(raw), stdout=output)
        return code, json.loads(output.getvalue()), factory

    def test_native_uses_only_fixed_endpoint_through_existing_proxy(self):
        expected = {"enabled": True, "work_id": "fictional", "inputs": [], "output_dir": "/fixture/outputs"}
        peer = Peer(expected)
        with patch.object(socket, "create_connection", return_value=peer) as connect:
            with patch.dict(os.environ, {"CC_FILE_ACTION_TOKEN": "fixture-token",
                    "CC_FILE_TRANSPORT": "native", "HTTP_PROXY": "http://127.0.0.1:43123",
                    "NO_PROXY": "*"}, clear=True):
                output = io.StringIO()
                code = file_tool.main(["work-context"], stdin=io.BytesIO(b'{}'), stdout=output)
        self.assertEqual((code, json.loads(output.getvalue())), (0, expected))
        self.assertEqual(connect.call_args.args[0], ("127.0.0.1", 43123))
        self.assertTrue(bytes(peer.sent).startswith(b'POST http://127.0.0.1:18743/tool HTTP/1.1'))
        self.assertIn(b'Authorization: Bearer fixture-token', peer.sent)
        self.assertTrue(peer.closed)
        for proxy in ('', 'http://remote.invalid:80', 'https://127.0.0.1:80',
                      'http://user@localhost:80', 'http://user:@localhost:80',
                      'http://user:%0d%0asecret@localhost:80', 'http://user:password@remote.invalid:80',
                      'http://localhost:80/other',
                      'http://localhost:80?endpoint=other', 'http://localhost:99999'):
            code, result, factory = self.invoke(['work-context'], b'{}', Peer({}),
                {'CC_FILE_TRANSPORT':'native', 'HTTP_PROXY':proxy})
            self.assertEqual((code,result['code']), (1,'file_transport_disabled'))
            factory.assert_not_called()

    def test_native_authenticated_proxy_keeps_proxy_and_session_credentials_separate(self):
        expected = {"enabled": True, "work_id": "fictional", "inputs": [], "output_dir": "/fixture/outputs"}
        peer = Peer(expected)
        with (patch.object(socket, "create_connection", return_value=peer) as connect,
              patch.dict(os.environ, {'CC_FILE_ACTION_TOKEN': 'fixture-token',
                  'CC_FILE_TRANSPORT': 'native', 'HTTP_PROXY': 'http://srt:fixture%2Bproxy@127.0.0.1:43123',
                  'NO_PROXY': '*'}, clear=True)):
            output = io.StringIO()
            code = file_tool.main(["work-context"], stdin=io.BytesIO(b'{}'), stdout=output)
            result = json.loads(output.getvalue())
        self.assertEqual((code, result), (0, expected))
        request = bytes(peer.sent)
        self.assertEqual(connect.call_args.args[0], ("127.0.0.1", 43123))
        self.assertTrue(request.startswith(b'POST http://127.0.0.1:18743/tool HTTP/1.1'))
        self.assertIn(b'Proxy-Authorization: Basic ' + base64.b64encode(b'srt:fixture+proxy'), request)
        self.assertIn(b'Authorization: Bearer fixture-token', request)
        self.assertNotIn(b'fixture%2Bproxy', request)
        self.assertTrue(peer.closed)

    @unittest.skipUnless(os.name == 'posix', 'POSIX publication')
    def test_native_publishes_selected_work_with_employee_cwd_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            (root / 'outputs').mkdir()
            artifact = root / 'outputs' / 'result.docx'
            artifact.write_bytes(b'fictional-output')
            artifact.chmod(0o600)
            previous = os.getcwd()
            with patch.dict(os.environ, {'CC_FILE_TRANSPORT':'native', 'CC_FILE_WORK_ROOT':str(root)}):
                file_tool.prepare_delivery({'path':'result.docx', 'sha256':hashlib.sha256(artifact.read_bytes()).hexdigest()})
                self.assertEqual(stat.S_IMODE(artifact.stat().st_mode), 0o640)
                self.assertEqual(previous, os.getcwd())
            with patch.dict(os.environ, {'CC_FILE_TRANSPORT':'native', 'CC_FILE_WORK_ROOT':''}):
                with self.assertRaises(ValueError):
                    file_tool.prepare_delivery({'path':'result.docx'})

    def test_only_fixed_socket_and_business_envelope_are_sent(self):
        expected = {"enabled": True, "work_id": "fictional", "inputs": [], "output_dir": "/fixture/outputs"}
        peer = Peer(expected)
        code, result, factory = self.invoke(["work-context"], b"{}", peer)
        self.assertEqual((0, expected), (code, result))
        factory.assert_called_once_with(getattr(socket, "AF_UNIX", 99), socket.SOCK_STREAM)
        self.assertEqual(file_tool.SOCKET, peer.address)
        headers, body = bytes(peer.sent).split(b"\r\n\r\n", 1)
        self.assertTrue(headers.startswith(b"POST /tool HTTP/1.1\r\n"))
        self.assertIn(b"Authorization: Bearer fixture-token", headers)
        self.assertEqual({"command": "work-context", "input": {}}, json.loads(body))
        self.assertTrue(peer.closed)

    def test_overrides_invalid_json_and_missing_capability_never_connect(self):
        cases = [(["send"], b"{}", None), (["work-context", "--socket", "/other"], b"{}", None),
                 (["work-context"], b'{"recipient":"other"}', None),
                 (["file-status"], b'{"work_id":"a","delivery_id":"b","root":"/other"}', None),
                 (["work-context"], b'{"x":1,"x":2}', None),
                 (["work-context"], b'{} {}', None), (["work-context"], b'null', None),
                 (["work-context"], b'{}', {"CC_FILE_ACTION_TOKEN": "bad\r\nsecret"}),
                 (["work-context"], b'{}', {"CC_ACTION_TOOLS_SOCKET": "/other/socket"}),
                 (["work-context"], b'{}', {"CC_ACTION_TOOLS_SOCKET": ""})]
        for command, raw, env in cases:
            with self.subTest(command=command, raw_size=len(raw)):
                code, _, factory = self.invoke(command, raw, Peer({}), env)
                self.assertNotEqual(0, code)
                factory.assert_not_called()

    def test_durable_statuses_are_preserved_and_network_is_never_retried(self):
        raw = b'{"work_id":"fictional","delivery_id":"delivery-a"}'
        for status in ("submitted", "accepted", "failed", "unknown"):
            expected = {"delivery_id": "delivery-a", "status": status, "version": 1}
            if status == "accepted":
                expected["message_receipt"] = "message-a"
            code, result, factory = self.invoke(["file-status"], raw, Peer(expected))
            self.assertEqual((0, expected), (code, result))
            self.assertEqual(1, factory.call_count)
        peer = Peer({})
        peer.sendall = lambda _data: (_ for _ in ()).throw(OSError("private secret path"))
        code, result, factory = self.invoke(["file-status"], raw, peer)
        self.assertEqual((1, {"status": "unavailable", "code": "outcome_unconfirmed"}), (code, result))
        self.assertEqual(1, factory.call_count)
        self.assertTrue(peer.closed)

    def test_redirect_and_false_acceptance_are_not_receipts(self):
        for peer in (Peer({"status": "accepted"}, 302), Peer({"status": "accepted"}),
                     Peer({"status": "accepted"}, 500), Peer({"enabled": False})):
            code, result, factory = self.invoke(["work-context"], b"{}", peer)
            self.assertEqual(1, code)
            self.assertEqual("invalid_response", result["code"])
            self.assertEqual(1, factory.call_count)


if __name__ == "__main__":
    unittest.main()
