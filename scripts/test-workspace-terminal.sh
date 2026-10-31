#!/usr/bin/env bash
# Exercise a Homebrew ncurses application in a real PTY with Ghostty's TERM.
set -euo pipefail
image=${1:?Usage: test-workspace-terminal.sh IMAGE}
output=$(mktemp)
trap 'rm -f "$output"' EXIT
if docker run --rm --tty --env TERM=xterm-ghostty "$image" /bin/bash -lc '
  set -e
  /usr/bin/infocmp -x "$TERM" >/dev/null
  brew install htop >/tmp/homebrew-htop.log 2>&1 || { cat /tmp/homebrew-htop.log; exit 1; }
  timeout 3 htop
' >"$output" 2>&1; then
  status=0
else
  status=$?
fi
# A healthy htop remains in its event loop until timeout terminates it. Missing
# terminfo or other startup failures exit early instead of returning 124.
if [ "$status" -ne 124 ]; then
  cat "$output"
  printf 'Homebrew htop failed to remain running with TERM=xterm-ghostty (exit %s).\n' "$status" >&2
  exit 1
fi
printf 'Homebrew htop initialized with TERM=xterm-ghostty.\n'
