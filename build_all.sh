#!/bin/bash
cd /path/to/ipxe
echo "[$(date +%T)] build_all START"
echo "[$(date +%T)] probing Docker Hub: golang:1.23-bookworm"
timeout 90 docker pull golang:1.23-bookworm 2>&1 | tail -6
echo "PULL_RC=${PIPESTATUS[0]}"
echo "[$(date +%T)] compose build + run"
docker compose -f docker-compose.build.yml run --build --rm builder
echo "[$(date +%T)] build_all END rc=$?"
