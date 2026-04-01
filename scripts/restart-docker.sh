#!/bin/bash
set -e

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

docker_bin="${DOCKER_BIN:-$(command -v docker || true)}"
if [ -z "$docker_bin" ] && [ -x /opt/homebrew/bin/docker ]; then
  docker_bin=/opt/homebrew/bin/docker
fi
if [ -z "$docker_bin" ] && [ -x /usr/local/bin/docker ]; then
  docker_bin=/usr/local/bin/docker
fi
if [ -z "$docker_bin" ]; then
  echo "ERROR: docker CLI was not found on PATH."
  exit 1
fi

echo "==> Force killing all Docker processes..."
pkill -9 -f 'docker' 2>/dev/null || true
pkill -9 -f 'Docker' 2>/dev/null || true
pkill -9 -f 'com.docker' 2>/dev/null || true
sleep 2

remaining=$(ps aux | grep -i docker | grep -v grep | wc -l | tr -d ' ')
if [ "$remaining" -gt 0 ]; then
  echo "WARNING: $remaining Docker processes still running, retrying..."
  pkill -9 -f 'docker' 2>/dev/null || true
  pkill -9 -f 'Docker' 2>/dev/null || true
  pkill -9 -f 'com.docker' 2>/dev/null || true
  sleep 2
fi
echo "    All Docker processes killed."

echo "==> Cleaning up socket and lock files..."
rm -f ~/Library/Containers/com.docker.docker/Data/vms/0/*.sock 2>/dev/null
rm -f ~/Library/Containers/com.docker.docker/Data/*.lock 2>/dev/null
rm -f ~/.docker/run/*.sock 2>/dev/null
echo "    Done."

echo "==> Starting Docker Desktop..."
open -a Docker

echo "==> Waiting for Docker daemon to become responsive..."
for i in $(seq 1 30); do
  if "$docker_bin" info &>/dev/null; then
    echo "    Docker is ready!"
    exit 0
  fi
  sleep 2
done

echo "ERROR: Docker did not become responsive within 60 seconds."
exit 1
