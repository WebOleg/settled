#!/usr/bin/env bash
# Checks every tracked text file against the documentation and formatting rules in
# ENGINEERING.md. Prints one line per violation as path:line: reason and exits 1 if
# there is any. Works with the bash 3.2 that ships with macOS.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

EM_DASH=$'\xe2\x80\x94'
CR=$'\r'
violations=0

report() {
  printf '%s: %s\n' "$1" "$2"
  violations=$((violations + 1))
}

is_text() {
  # git decides: a file with a binary attribute or binary content is skipped.
  [ -z "$(git diff --no-index --numstat /dev/null "$1" 2>/dev/null | awk '$1 == "-"')" ]
}

while IFS= read -r -d '' file; do
  [ -f "$file" ] || continue
  is_text "$file" || continue

  if LC_ALL=C grep -q "$CR" "$file"; then
    report "$file" "CRLF line ending"
  fi

  if [ -s "$file" ] && [ -n "$(tail -c 1 "$file")" ]; then
    report "$file" "no newline at end of file"
  fi

  case "$file" in
    *.md)
      while IFS=: read -r line _; do
        report "$file:$line" "em dash"
      done < <(LC_ALL=C grep -n "$EM_DASH" "$file" || true)

      # Relative links must point at a file or folder that exists.
      dir="$(dirname "$file")"
      while IFS=: read -r line link; do
        target="${link%%#*}"
        [ -n "$target" ] || continue
        case "$target" in
          http://*|https://*|mailto:*) continue ;;
        esac
        if [ ! -e "$dir/$target" ]; then
          report "$file:$line" "broken link: $target"
        fi
      done < <(grep -no '](\([^)]*\))' "$file" | sed 's/^\([0-9]*\):](\(.*\))$/\1:\2/' || true)
      ;;
    *)
      while IFS=: read -r line _; do
        report "$file:$line" "trailing whitespace"
      done < <(grep -n $'[ \t]$' "$file" || true)
      ;;
  esac
done < <(git ls-files -z)

if [ "$violations" -gt 0 ]; then
  printf 'check-docs: %d violation(s)\n' "$violations"
  exit 1
fi
echo "check-docs: ok"
