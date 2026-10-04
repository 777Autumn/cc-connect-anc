#!/bin/sh
#
# runas_probe.sh — leak-audit probe for the run_as_user sandbox.
#
# Invoked by core.RunIsolationProbe via `sudo -n -iu <target> -- /bin/sh -s`
# with this script piped on stdin. Writes a stable, parseable report to
# stdout. Every line is prefixed with a section tag so the Go side can
# read it deterministically without shell quoting hazards.
#
# This script must NOT read or echo any secret material. It reports
# existence and access, never contents.
#
# It is also intentionally written in POSIX /bin/sh, not bash, so it runs
# on macOS and BusyBox-based systems without relying on bashisms.
#
# Environment inputs (set by the Go caller via the shell invocation):
#   CC_PROBE_WORKDIR        — project work_dir (required)
#   CC_PROBE_OTHER_USERS    — space-separated list of other run_as_user
#                             values configured in the same cc-connect
#                             instance, for cross-user denial tests
#   CC_PROBE_SUPERVISOR     — the supervisor Unix username (for denial test)

set -u

emit() {
    # emit TAG VALUE ...
    printf '%s\n' "$*"
}

account_home() {
    if [ "$(uname -s)" = Darwin ]; then
        # Directory Services may not resolve an existing account immediately
        # after boot. Retry only this read; unresolved accounts still fail closed.
        for attempt in 1 2 3; do
            if record=$(/usr/bin/dscl . -read "/Users/$1" NFSHomeDirectory 2>/dev/null); then
                home=$(printf '%s\n' "$record" | sed -n 's/^NFSHomeDirectory: //p')
                case "$home" in
                    /*) printf '%s\n' "$home"; return 0 ;;
                esac
            fi
            [ "$attempt" -eq 3 ] || sleep 1
        done
        return 1
    else
        getent passwd "$1" 2>/dev/null | cut -d: -f6
    fi
}

emit "BEGIN probe-version=1"

# ---------------------------------------------------------------- identity
emit "ID $(id 2>/dev/null || echo unknown)"
emit "WHOAMI $(whoami 2>/dev/null || echo unknown)"
emit "GROUPS $(id -Gn 2>/dev/null || echo unknown)"
emit "UMASK $(umask 2>/dev/null || echo unknown)"
emit "PWD $(pwd 2>/dev/null || echo unknown)"
emit "HOME ${HOME:-unknown}"
emit "SHELL ${SHELL:-unknown}"

# ---------------------------------------------------------------- work_dir
if [ -n "${CC_PROBE_WORKDIR:-}" ]; then
    emit "WORKDIR_PATH ${CC_PROBE_WORKDIR}"
    if [ -d "${CC_PROBE_WORKDIR}" ]; then
        emit "WORKDIR_EXISTS yes"
    else
        emit "WORKDIR_EXISTS no"
    fi
    if [ -r "${CC_PROBE_WORKDIR}" ]; then
        emit "WORKDIR_READABLE yes"
    else
        emit "WORKDIR_READABLE no"
    fi
    if [ -w "${CC_PROBE_WORKDIR}" ]; then
        emit "WORKDIR_WRITABLE yes"
    else
        emit "WORKDIR_WRITABLE no"
    fi
fi

# ----------------------------------------------------- target user's config
# Things the target user is supposed to have in THEIR home. We just check
# existence — not contents.
for f in \
    "${HOME}/.claude/settings.json" \
    "${HOME}/.claude.json" \
    "${HOME}/.claude/plugins" \
    "${HOME}/.pgpass" \
    "${HOME}/keys" \
    "${HOME}/.ssh" \
    "${HOME}/.config/gh"
do
    if [ -e "$f" ]; then
        emit "TARGET_HAS $f"
    else
        emit "TARGET_MISSING $f"
    fi
done

# ------------------------------------------- cross-user denial tests
# For each OTHER configured run_as_user, try to READ a file inside their
# home. Expected outcome: denied. We report DENIED or LEAKED per path.
if [ -n "${CC_PROBE_OTHER_USERS:-}" ]; then
    for other in ${CC_PROBE_OTHER_USERS}; do
        if [ "$other" = "$(whoami)" ]; then
            continue
        fi
        other_home=$(account_home "$other")
        case "$other_home" in /*) ;; *)
            emit "CROSS_UNKNOWN $other"
            continue
        esac
        for f in \
            "$other_home/.codex/auth.json" \
            "$other_home/.claude/settings.json" \
            "$other_home/.claude.json" \
            "$other_home/.ssh/id_rsa" \
            "$other_home/.ssh/id_ed25519" \
            "$other_home/.pgpass" \
            "$other_home/keys"
        do
            if [ ! -e "$f" ]; then
                emit "CROSS_MISSING ${other} ${f}"
                continue
            fi
            if [ -r "$f" ]; then
                emit "CROSS_LEAKED ${other} ${f}"
            else
                emit "CROSS_DENIED ${other} ${f}"
            fi
        done
    done
fi

# ---------------------------------------------------- supervisor denial
if [ -n "${CC_PROBE_SUPERVISOR:-}" ]; then
    sup=${CC_PROBE_SUPERVISOR}
    if [ "$sup" != "$(whoami)" ]; then
        sup_home=$(account_home "$sup")
        case "$sup_home" in /*)
            for f in \
                "$sup_home/.codex/auth.json" \
                "$sup_home/.claude/settings.json" \
                "$sup_home/.claude.json" \
                "$sup_home/.ssh/id_rsa" \
                "$sup_home/.ssh/id_ed25519" \
                "$sup_home/.pgpass"
            do
                if [ ! -e "$f" ]; then
                    emit "SUPERVISOR_MISSING ${f}"
                    continue
                fi
                if [ -r "$f" ]; then
                    emit "SUPERVISOR_LEAKED ${f}"
                else
                    emit "SUPERVISOR_DENIED ${f}"
                fi
            done
            ;;
        *) emit "SUPERVISOR_UNKNOWN $sup" ;;
        esac
    fi
fi

emit "END probe-version=1"
