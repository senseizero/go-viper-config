#!/usr/bin/env bash
# audit-inert-env.sh — which deployed env vars start working when you bump.
#
# Up to v1.3.0 an env override only reached the struct when its key already
# existed in the service's YAML: viper unmarshals AllSettings, and a key no
# config file declares is not in it. Every <PREFIX>_* variable set on the
# deployment whose key is missing from the YAML is therefore INERT today and
# starts taking effect once per-key env binding lands.
#
# Usually that is the whole point — someone set the variable expecting it to
# work. Read the list anyway: it is the complete set of behaviour changes a
# version bump brings with it.
#
#   usage: hack/audit-inert-env.sh <repo-dir> <deployment-values-dir> [PREFIX]
#   e.g.   hack/audit-inert-env.sh ~/projects/kensaku ~/projects/sensei-kube/services/kensaku ZERO
set -uo pipefail
[ $# -ge 2 ] || { sed -n '2,16p' "$0"; exit 2; }
repo="$1"; vals="$2"; prefix="${3:-ZERO}"

keys=$( { grep -rhoE '^[[:space:]]*[a-zA-Z][a-zA-Z0-9_]*:' "$repo" --include='config*.y*ml' 2>/dev/null
          grep -rhoE '[{,][[:space:]]*[a-zA-Z][a-zA-Z0-9_]*:' "$repo" --include='config*.y*ml' 2>/dev/null; } \
        | tr -d ' :{,' | tr '[:upper:]' '[:lower:]' | sort -u )

grep -rhoE "^[[:space:]]+${prefix}_[A-Z0-9_]+:" "$vals" 2>/dev/null \
  | tr -d ' :' | sort -u \
  | while read -r ev; do
      leaf=$(printf '%s' "${ev##*_}" | tr '[:upper:]' '[:lower:]')
      printf '%s\n' "$keys" | grep -qx "$leaf" || echo "INERT TODAY: $ev"
    done

echo
echo "Matching is on the variable's last segment against the YAML key names,"
echo "so snake_case keys and repeated leaf names produce false positives."
