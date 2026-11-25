#!/bin/sh
# Installs Ubuntu packages on a GitHub-hosted runner. The runner reads its apt
# mirrors from apt-mirrors.txt, and every one is named under Canonical's DNS, which
# has failed while the mirrors themselves stayed up. When the first mirror's name
# does not resolve, independent mirrors of the same archive replace the list. apt
# still checks the archive's signatures with the Ubuntu keyring on the runner.
set -eu
mirrors=/etc/apt/apt-mirrors.txt
primary=$(sed -n '1s|^[^:]*://\([^/]*\)/.*|\1|p' "$mirrors" 2>/dev/null || true)
if [ -n "$primary" ] && ! timeout 30 getent hosts "$primary" >/dev/null; then
  echo "::warning::$primary does not resolve; installing from independent Ubuntu mirrors"
  printf 'https://mirrors.edge.kernel.org/ubuntu/\tpriority:1\nhttps://mirror.math.princeton.edu/pub/ubuntu/\tpriority:2\n' |
    sudo tee "$mirrors" >/dev/null
fi
sudo apt-get update
sudo apt-get install -y "$@"
