"""Offline startup contract: real CRM CLI/auth/Connections, existing CRM fixtures.

Only protected configuration and external Lark CLI calls are injected. Responses
are emitted by the paired CRM source's host-connection-config implementation.
"""
import argparse
import hashlib
from pathlib import Path
import sys
from unittest import mock


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True)
    parser.add_argument("--mode", choices=("legacy", "personal"), required=True)
    args = parser.parse_args()
    root = Path(args.root)
    if not root.is_absolute() or not (root / "myanc_crm" / "connections.py").is_file():
        raise ValueError("paired CRM source is required")
    sys.path[:0] = [str(root), str(root / "tests")]
    from myanc_crm import cli
    from myanc_crm.connections import Connections
    from test_connections import ConnectionTests
    from test_personal_apps import PersonalAppConnectionsTests

    fixture = ConnectionTests()
    fixture.setUp()
    try:
        config = fixture.config
        if args.mode == "personal":
            personal = PersonalAppConnectionsTests()
            personal.fixture = fixture
            config, _ = personal.migrated()
        config["host_secret_sha256"] = hashlib.sha256(
            b"offline-connection-contract-secret-000000001").hexdigest()
        with (
            mock.patch.object(cli, "_config_path", return_value=Path(fixture.temp.name) / "fixture-unused"),
            mock.patch.object(cli, "_load_config", return_value=config),
            mock.patch.object(cli, "Connections", side_effect=lambda cfg: Connections(cfg, run=fixture.fake)),
        ):
            return cli.main(["host-connection-config"])
    finally:
        fixture.doCleanups()


if __name__ == "__main__":
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
    raise SystemExit(main())
