#!/bin/bash
# © 2026 Platform Engineering Labs Inc.
# SPDX-License-Identifier: Apache-2.0
#
# clean-environment.sh — manages the ephemeral Postgres container that
# atlas plugin conformance tests target. Runs both before and after
# `make conformance-test`.
#
# Behavior:
#   - If no container labeled formae-plugin-atlas-conformance is running:
#     start one publishing 5432 on the host's FORMAE_ATLAS_TEST_PORT
#     (default 5499). Wait for readiness.
#   - If one is already running: drop the conformance DB and recreate
#     it so each test run starts clean (the testdata PKL files declare
#     the DB name as `conformance`).
#   - Idempotent. Skips silently on platforms without docker.

set -euo pipefail

PORT="${FORMAE_ATLAS_TEST_PORT:-5499}"
CONTAINER_NAME="formae-plugin-atlas-conformance"
DB_USER="postgres"
DB_PASS="conformance"
DB_NAME="conformance"

if ! command -v docker >/dev/null 2>&1; then
  echo "clean-environment: docker not installed; skipping Postgres setup" >&2
  echo "(integration tests using testcontainers will skip; conformance tests will fail until docker is available)" >&2
  exit 0
fi

# Start the container if it isn't already running.
if ! docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
  echo "clean-environment: starting Postgres at 127.0.0.1:${PORT}"
  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  docker run -d --rm \
    --name "${CONTAINER_NAME}" \
    -e "POSTGRES_PASSWORD=${DB_PASS}" \
    -e "POSTGRES_DB=${DB_NAME}" \
    -p "${PORT}:5432" \
    postgres:15-alpine >/dev/null

  # Wait for readiness.
  for i in $(seq 1 60); do
    if docker exec "${CONTAINER_NAME}" pg_isready -U "${DB_USER}" >/dev/null 2>&1; then
      echo "clean-environment: Postgres ready after ${i}s"
      break
    fi
    sleep 1
    if [[ "$i" -eq 60 ]]; then
      echo "clean-environment: Postgres did not become ready" >&2
      docker logs "${CONTAINER_NAME}" 2>&1 | tail -20 || true
      exit 1
    fi
  done
fi

# Wipe and recreate the conformance database so each run starts fresh.
echo "clean-environment: recreating database '${DB_NAME}'"
docker exec "${CONTAINER_NAME}" psql -U "${DB_USER}" -d postgres -c \
  "DROP DATABASE IF EXISTS ${DB_NAME};" >/dev/null
docker exec "${CONTAINER_NAME}" psql -U "${DB_USER}" -d postgres -c \
  "CREATE DATABASE ${DB_NAME};" >/dev/null

echo "clean-environment: ready"
