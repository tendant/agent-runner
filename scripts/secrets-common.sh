#!/usr/bin/env bash
# Shared helpers for the secrets-* scripts. Sourced, not executed.

set -euo pipefail

ENV_FILE="${ENV_FILE:-.env}"
SOPS_FILE="${SOPS_FILE:-.env.sops}"
SOPS_CONFIG="${SOPS_CONFIG:-.sops.yaml}"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# SOPS looks for the default age identity under os.UserConfigDir(), which on
# macOS is ~/Library/Application Support -- not ~/.config. Resolve it here so
# the same scripts work on macOS and Linux.
resolve_age_key() {
  [ -n "${SOPS_AGE_KEY_FILE:-}" ] && return 0
  [ -n "${SOPS_AGE_KEY:-}" ] && return 0
  local c
  for c in \
    "$HOME/.config/sops/age/keys.txt" \
    "$HOME/Library/Application Support/sops/age/keys.txt"
  do
    if [ -f "$c" ]; then
      export SOPS_AGE_KEY_FILE="$c"
      return 0
    fi
  done
  return 0
}

require_tools() {
  command -v sops >/dev/null 2>&1 || die "sops is not installed (brew install sops)"
}

# Recipients the policy file says SHOULD have access.
desired_recipients() {
  [ -f "$SOPS_CONFIG" ] || die "$SOPS_CONFIG not found"
  grep -oE 'age1[0-9a-z]{50,}' "$SOPS_CONFIG" | sort -u
}

# Recipients that ARE currently able to unwrap this file's data key.
actual_recipients() {
  [ -f "$SOPS_FILE" ] || return 0
  grep -oE '^sops_age__list_[0-9]+__map_recipient=age1[0-9a-z]{50,}' "$SOPS_FILE" \
    | sed 's/.*=//' | sort -u
}

# Verify a candidate encrypted file decrypts and carries the same key=value
# pairs as the plaintext source. Guards against committing a file that cannot
# be decrypted back, or that silently lost a variable.
verify_roundtrip() {
  local candidate="$1" source="$2" plain
  plain="$(mktemp)"
  # shellcheck disable=SC2064
  trap "rm -f '$plain'" RETURN

  sops decrypt --input-type dotenv --output-type dotenv "$candidate" > "$plain" 2>/dev/null \
    || { echo "  decrypt of candidate failed" >&2; return 1; }

  ENV_A="$source" ENV_B="$plain" python3 -c '
import os, re, sys

def parse(path):
    out = {}
    with open(path, errors="replace") as fh:
        for line in fh:
            m = re.match(r"^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$", line.rstrip("\n"))
            if m and not m.group(1).startswith("sops_"):
                out[m.group(1)] = m.group(2)
    return out

a = parse(os.environ["ENV_A"])
b = parse(os.environ["ENV_B"])
bad = [k for k in set(a) | set(b) if a.get(k) != b.get(k)]
if bad:
    for k in sorted(bad):
        print(f"  mismatch: {k}", file=sys.stderr)
    sys.exit(1)
print(f"  {len(a)} variables verified")
'
}
