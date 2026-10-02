#!/bin/sh
# Screenshots of Chokominto as it is in the working tree, with demo
# listens. See .claude/skills/screenshots/SKILL.md for when and how.
#
#   dev/shots/run.sh "/album/1" "/history" "/settings#reading" ...
#
# Builds a static binary in the Go container, then runs shots.py in the
# Playwright image with no network at all: the demo server would otherwise
# look up pictures on iTunes and Deezer by itself. Pictures to upload come
# from internal/artwork/testdata (cover.webp is the WebP there).
# Screenshots go to $OUT (default chokominto-screenshots next to the repo), which
# is outside the repo. The browser image is built from the Containerfile
# here the first time.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
out=${OUT:-$(dirname "$repo")/chokominto-screenshots}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
image=localhost/chokominto-shots
if ! podman image exists "$image"; then
  podman build -t "$image" "$here"
fi
mkdir -p "$out"
rm -f "$out"/[0-9][0-9].png

podman run --rm -v "$repo":/src:Z -v chokominto-gocache:/root/.cache -v chokominto-gomod:/go/pkg/mod -w /src \
  docker.io/library/golang:1.27.1 sh -c 'CGO_ENABLED=0 go build -o /tmp/c ./cmd/chokominto && cat /tmp/c' > "$out/chokominto"
chmod +x "$out/chokominto"

cp "$here/shots.py" "$here/listens.json" "$work/"
cp "$repo/internal/artwork/testdata/blue-purple-pink.lossy.webp" "$work/cover.webp"
if [ -n "${LISTENS:-}" ]; then cp "$LISTENS" "$work/listens.json"; fi

podman run --rm --network=none -e WIDTH="${WIDTH:-1280}" \
  -v "$work":/work:Z -v "$out":/out:Z --entrypoint python3 "$image" /work/shots.py "$@"
rm -f "$out/chokominto"
