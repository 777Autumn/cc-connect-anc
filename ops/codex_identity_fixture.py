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
import tempfile


def run(*args):
    subprocess.run(args, check=True, timeout=30)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--test-binary", required=True)
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
        stream.write(f"{gateway.pw_name} ALL=({model.pw_name}) NOPASSWD: /usr/bin/true\n"
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
    return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
