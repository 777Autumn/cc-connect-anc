#!/usr/bin/env python3
"""One-shot Linux/macOS CI setup; the actual adapter runs as ordinary users."""
import argparse
import grp
import json
import os
from pathlib import Path
import platform
import pwd
import shutil
import subprocess
import sys
import tempfile


def run(*args):
    subprocess.run(args, check=True, timeout=30)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--test-binary", required=True)
    parser.add_argument("--file-test-binary")
    parser.add_argument("--startup-test-binary")
    args = parser.parse_args()
    if os.geteuid() != 0 or os.environ.get("GITHUB_ACTIONS") != "true":
        raise SystemExit("requires authorized disposable GitHub runner setup")
    mac = platform.system() == "Darwin"
    if not mac and platform.system() != "Linux":
        raise SystemExit("unsupported CI platform")
    names = ("_anccigw", "_anccimodel", "_ancciother")
    sudoers = Path("/etc/sudoers.d/anc-codex-ci")
    if sudoers.exists():
        raise SystemExit("fixture rule already exists")
    for name in names:
        try:
            pwd.getpwnam(name)
        except KeyError:
            pass
        else:
            raise SystemExit("fixture identity already exists")
        try:
            grp.getgrnam(name)
        except KeyError:
            pass
        else:
            raise SystemExit("fixture group already exists")
    # Hosted runners are discarded after the job. Keep failed setup in place
    # for diagnosis; this script never replaces accounts or retries installation.
    root = Path(tempfile.mkdtemp(prefix="anc-codex-ci-")).resolve()
    root.chmod(0o755)
    for name in names:
        home = root / name
        home.mkdir(mode=0o700)
        if mac:
            uid = next(n for n in range(700, 1000)
                       if n not in {p.pw_uid for p in pwd.getpwall()} and n not in {g.gr_gid for g in grp.getgrall()})
            for key, value in (("PrimaryGroupID", str(uid)),):
                run("dscl", ".", "-create", f"/Groups/{name}", key, value)
            for key, value in (("UniqueID", str(uid)), ("PrimaryGroupID", str(uid)),
                               ("NFSHomeDirectory", str(home)), ("UserShell", "/bin/sh"), ("IsHidden", "1")):
                run("dscl", ".", "-create", f"/Users/{name}", key, value)
        else:
            run("useradd", "--system", "--user-group", "--no-create-home", "--home-dir", str(home), "--shell", "/bin/sh", name)
        identity = pwd.getpwnam(name)
        os.chown(home, identity.pw_uid, identity.pw_gid)
        secret = home / "private.txt"
        secret.write_text("synthetic private fixture\n", encoding="utf-8")
        secret.chmod(0o600)
        os.chown(secret, identity.pw_uid, identity.pw_gid)
    gateway, model, other = (pwd.getpwnam(name) for name in names)
    work = root / "work"
    work.mkdir(mode=0o700)
    os.chown(work, model.pw_uid, model.pw_gid)
    home = Path(model.pw_dir) / ".codex"
    home.mkdir(mode=0o700)
    os.chown(home, model.pw_uid, model.pw_gid)
    binary = root / "codex.test"
    shutil.copyfile(args.test_binary, binary)
    binary.chmod(0o555)
    with sudoers.open("x", encoding="utf-8") as stream:
        stream.write(f'Defaults:{gateway.pw_name} env_keep += "CC_FILE_ACTION_TOKEN MYANC_CRM_ACTION_TOKEN"\n'
                     f"{gateway.pw_name} ALL=({model.pw_name}) NOPASSWD: /usr/bin/true\n"
                     f"{gateway.pw_name} ALL=({model.pw_name}) NOPASSWD: /usr/bin/test *, /bin/test *, /usr/bin/find *, /bin/sh -s\n"
                     f"{gateway.pw_name} ALL=({model.pw_name}) NOPASSWD: SETENV: /bin/sh -c *\n")
    sudoers.chmod(0o440)
    run("visudo", "-cf", str(sudoers))
    settings = root / "fixture.json"
    settings.write_text(json.dumps({"Model": model.pw_name, "Other": other.pw_name, "Work": str(work), "Home": str(home),
        "GatewayUID": gateway.pw_uid, "ModelUID": model.pw_uid,
        "Forbidden": os.pathsep.join(str(Path(u.pw_dir) / "private.txt") for u in (gateway, other))}), encoding="utf-8")
    settings.chmod(0o444)
    result = subprocess.run(["sudo", "-n", "-u", gateway.pw_name, "--", "/usr/bin/env", "-i", "PATH=/usr/bin:/bin:/usr/sbin:/sbin",
        f"HOME={gateway.pw_dir}", f"ANC_CODEX_IDENTITY_FIXTURE={settings}", str(binary),
        "-test.run=^TestCodexRunAsOrdinaryIdentities$", "-test.v", "-test.timeout=90s"], cwd=root, timeout=100)
    if result.returncode or not args.file_test_binary:
        return result.returncode
    # Reuse the existing file journey with the same ordinary identities.
    # Only setup is privileged; the gateway creates work and snapshots itself.
    if mac:
        run("dseditgroup", "-o", "edit", "-a", gateway.pw_name, "-t", "user", model.pw_name)
    else:
        run("usermod", "-a", "-G", model.pw_name, gateway.pw_name)
    files = root / "files"
    files.mkdir(mode=0o750)
    os.chown(files, gateway.pw_uid, model.pw_gid)
    for name in ("works", "tmp"):
        directory = files / name
        directory.mkdir(mode=0o750)
        os.chown(directory, gateway.pw_uid, model.pw_gid)
    for name in ("native_file_fixture.py", "file_tool.py"):
        destination = root / name
        shutil.copyfile(Path(__file__).with_name(name), destination)
        destination.chmod(0o555)
    binary = root / "files.test"
    shutil.copyfile(args.file_test_binary, binary)
    binary.chmod(0o555)
    settings = root / "file-fixture.json"
    settings.write_text(json.dumps({"Root": str(files), "Model": model.pw_name,
        "GatewayUID": gateway.pw_uid, "ModelUID": model.pw_uid, "EmployeeWork": str(work),
        "NativeCLI": str(root / "native_file_fixture.py"), "Client": str(root / "file_tool.py"),
        "Agent": "codex", "Python": sys.executable, "CodexHome": str(home)}), encoding="utf-8")
    settings.chmod(0o444)
    result = subprocess.run(["sudo", "-n", "-u", gateway.pw_name, "--", "/usr/bin/env", "-i",
        "PATH=/usr/bin:/bin:/usr/sbin:/sbin", f"HOME={gateway.pw_dir}", f"TMPDIR={files / 'tmp'}",
        f"CC_FILE_HANDOFF_FIXTURE={settings}", str(binary),
        "-test.run=^TestNativeFileWorkOrdinaryIdentities$", "-test.v", "-test.timeout=150s"],
        cwd=root, timeout=160).returncode
    if result or not args.startup_test_binary:
        return result
    # Gateway can resolve the exact auth path, but cannot list HOME or auth.
    # Its dedicated model-group membership grants traversal only here.
    Path(model.pw_dir).chmod(0o710)
    works = []
    for i in range(4):
        directory = root / f"shared-work-{i}"
        directory.mkdir(mode=0o750)
        os.chown(directory, model.pw_uid, model.pw_gid)
        works.append(str(directory))
    binary = root / "startup.test"
    shutil.copyfile(args.startup_test_binary, binary)
    binary.chmod(0o555)
    settings = root / "startup-fixture.json"
    settings.write_text(json.dumps({"Model": model.pw_name, "Home": str(home), "Root": str(files),
        "Works": works, "GatewayUID": gateway.pw_uid, "ModelUID": model.pw_uid}), encoding="utf-8")
    settings.chmod(0o444)
    return subprocess.run(["sudo", "-n", "-u", gateway.pw_name, "--", "/usr/bin/env", "-i",
        "PATH=/usr/bin:/bin:/usr/sbin:/sbin", f"HOME={gateway.pw_dir}", f"TMPDIR={files / 'tmp'}",
        f"ANC_STARTUP_FIXTURE={settings}", str(binary),
        "-test.run=^TestSharedStartupOrdinaryIdentities$", "-test.v", "-test.timeout=150s"],
        cwd=root, timeout=160).returncode


if __name__ == "__main__":
    raise SystemExit(main())
