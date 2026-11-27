#!/usr/bin/env bash
# Fails on any container image that is not pinned by digest: every `image:` in a tracked
# compose file, every image a tracked script or the Makefile names in a variable
# (IMG=, *_image=, *_IMAGE :=), and every FROM of a tracked Dockerfile. A tag can be
# moved to other content; a digest cannot. Also fails on an image built from this
# repository that leaves out a package its command is built from.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
bad=0
report() { echo "$1: not pinned by digest: $2"; bad=$((bad + 1)); }

while IFS= read -r f; do
  while IFS= read -r line; do
    ref="$(printf '%s' "$line" | sed -E 's/^[[:space:]]*image:[[:space:]]*//')"
    case "$ref" in *@sha256:*) ;; *) report "$f" "$ref" ;; esac
  done < <(grep -E '^[[:space:]]*image:' "$f" || true)
done < <(git ls-files '*.yaml' '*.yml')

while IFS= read -r f; do
  while IFS= read -r line; do
    case "$line" in *@sha256:*) ;; *) report "$f" "$line" ;; esac
  done < <(grep -E '^[[:space:]]*(IMG|[A-Za-z_]*_image)=|^[A-Z_]*_IMAGE[[:space:]]*:=' "$f" || true)
done < <(git ls-files '*.sh' 'Makefile')

# Base images of the tool images the repository builds.
while IFS= read -r f; do
  while IFS= read -r line; do
    ref="$(printf '%s' "$line" | awk '{ print $2 }')"
    case "$ref" in *@sha256:*) ;; *) report "$f" "$ref" ;; esac
  done < <(grep -iE '^[[:space:]]*FROM[[:space:]]' "$f" || true)
done < <(git ls-files '*Dockerfile')

# Each image built from this repository gets every package of the module its command
# is built from: the Dockerfile copies it and the .dockerignore lets it in. A package
# added to the command and left out fails here, not at the next deploy.
module="$(go list -m)"
missing=0
while IFS= read -r f; do
  cmd="$(sed -nE 's#^RUN .*go build .* \./(cmd/[a-z0-9-]+)$#\1#p' "$f" | head -1)"
  [ -n "$cmd" ] || continue
  while IFS= read -r dir; do
    grep -qxE "COPY $dir $dir" "$f" || { echo "$f: does not copy $dir, which $cmd is built from"; missing=$((missing + 1)); }
    grep -qx "!$dir" .dockerignore || { echo ".dockerignore: keeps out $dir, which $cmd is built from ($f)"; missing=$((missing + 1)); }
  done < <(go list -deps "./$cmd" | sed -n "s#^$module/##p")
done < <(git ls-files '*Dockerfile')

if [ "$bad" -gt 0 ] || [ "$missing" -gt 0 ]; then
  echo "check-images: $bad image(s) not pinned by digest, $missing package(s) missing from an image's build"
  exit 1
fi
echo "check-images: ok"
