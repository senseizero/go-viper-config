#!/usr/bin/env bash
# audit-zero-defaults.sh — what changes if you turn on WithStrictDefaults().
#
# Without strict defaults, validate() re-applies a field's `default` tag over
# any value isEmpty() calls empty, so an explicitly configured false / 0 / ""
# is silently replaced. Strict mode stops doing that, which changes the value
# a service reads WHEREVER something already sets one of those zero values.
#
# This lists exactly those places for one repo: every config field with a
# non-zero `default` tag that some YAML sets to a zero value.
#
#   usage: hack/audit-zero-defaults.sh <repo-dir> [extra-yaml-dir ...]
#   e.g.   hack/audit-zero-defaults.sh ~/projects/kumo ~/projects/sensei-kube/services/kumo
#
# Review every hit before enabling strict defaults: each one is a switch that
# is stuck at the default today and will start obeying the config.
set -uo pipefail
[ $# -ge 1 ] || { sed -n '2,15p' "$0"; exit 2; }
repo="$1"; shift
extra=("$@")

found=0
# key<TAB>default, from the struct tags
grep -rh 'default:"' "$repo" --include='*.go' 2>/dev/null \
  | sed -n 's/.*mapstructure:"\([a-zA-Z0-9_]*\)".*default:"\([^"]*\)".*/\1\t\2/p' \
  | sort -u \
  | while IFS=$'\t' read -r key def; do
      # a default that IS the zero value can never be re-applied wrongly
      case "$def" in 0|0.0|false|"") continue;; esac
      hits=$(grep -rn "[^a-zA-Z0-9_]${key}: *\(0\|0\.0\|false\|\"\"\|''\)\([,} ]\|$\)" \
               "$repo" --include='*.yaml' --include='*.yml' 2>/dev/null)
      ukey=$(printf '%s' "$key" | tr '[:lower:]' '[:upper:]')
      for dir in "${extra[@]:-}"; do
        [ -n "${dir:-}" ] && [ -d "$dir" ] || continue
        hits="$hits
$(grep -rn "_${ukey}: *[\"']\?\(0\|false\)[\"']\?$" "$dir" 2>/dev/null)"
      done
      hits=$(printf '%s' "$hits" | grep -v '^$')
      [ -n "$hits" ] || continue
      printf '\n%s (default=%s) is explicitly set to a zero value in:\n%s\n' "$key" "$def" "$hits"
      found=1
    done
echo
echo "Key names are matched without their parent path, so two different"
echo "'enabled' keys look alike — confirm each hit against its struct field."
