"""Offline paired context contract: real CRM CLI, comparison and approval ledger.

Reuse the CRM's fictional handoff fixture. Feishu IO and personal connection
selection are injected; this test does not claim live authorization or model evidence.
"""
import argparse
import hashlib
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest import mock


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', required=True)
    parser.add_argument('--stale', action='store_true')
    parser.add_argument('--command', choices=('host-tool', 'host-handoff-access'), default='host-tool')
    args = parser.parse_args()
    root = Path(args.root)
    if not root.is_absolute() or not (root / 'myanc_crm/banquet_context.py').is_file():
        raise ValueError('paired phase-one CRM source is required')
    sys.path[:0] = [str(root), str(root / 'tests')]
    from myanc_crm import cli, handoffs
    from test_handoffs import HandoffJourneyTests

    fixture = HandoffJourneyTests()
    fixture.setUp()
    try:
        with mock.patch.object(handoffs.secrets, 'token_urlsafe', return_value='paired_publication'):
            staged = fixture.proposal()
        fixture.decide(staged)
        if args.stale:
            fixture.store.rows[('banquets', fixture.business)]['estimated_tables'] = 23
        for index in range(12):
            identifier = 'context-followup-' + str(index)
            fixture.store.rows[('followups', identifier)] = dict(
                record_id=identifier, banquet_id=fixture.business,
                occurred_at=f'2026-10-01T{index:02d}:00:00+08:00', content='虚构配对跟进')
        fixture.config['host_secret_sha256'] = hashlib.sha256(
            b'offline-banquet-context-contract-000000001').hexdigest()
        fixture.config['allowed_actors']['other'] = dict(display_name='虚构非当事人', feishu_user_id='ou_other')
        personal = SimpleNamespace(route=lambda principal: 'private', guard=lambda *args: None)
        with (
            mock.patch.object(cli, '_config_path', return_value=Path('fixture-unused')),
            mock.patch.object(cli, '_load_config', return_value=fixture.config),
            mock.patch.object(cli, '_runtime', return_value=(fixture.backend, fixture.ledger)),
            mock.patch.object(cli, '_backend', return_value=fixture.backend),
            mock.patch.object(cli, 'Connections', return_value=personal),
            mock.patch.object(cli, 'connection_access', side_effect=lambda cfg, command, principal:
                              None if command == 'host-handoff-access' or principal['chat_id'] == 'shared-group' else personal),
            mock.patch.object(cli, 'PersonalReadRunner', return_value=fixture.backend.runner),
        ):
            return cli.main([args.command])
    finally:
        fixture.doCleanups()


if __name__ == '__main__':
    sys.stdin.reconfigure(encoding='utf-8')
    sys.stdout.reconfigure(encoding='utf-8')
    raise SystemExit(main())
