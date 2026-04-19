#!/usr/bin/env bash
# Kills and restarts the socat-proxy container to simulate a TCP drop.
set -e
docker compose -f "$(dirname "$0")/../docker-compose.yml" restart socat-proxy
