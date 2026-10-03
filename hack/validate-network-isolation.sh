#!/usr/bin/env bash
set -euo pipefail

: "${DOCKER_HOST:?Set DOCKER_HOST to the d2k Docker API endpoint before running this validation.}"

TEST_PREFIX="${D2K_TEST_PREFIX:-d2k-netiso-test}"
NET_A="${TEST_PREFIX}-a"
NET_B="${TEST_PREFIX}-b"
NET_INTERNAL="${TEST_PREFIX}-internal"
NET_LOCKED="${TEST_PREFIX}-locked"
SERVER_A="${TEST_PREFIX}-server-a"
SERVER_B="${TEST_PREFIX}-server-b"
PROBE="${TEST_PREFIX}-probe"
INTERNAL_PROBE="${TEST_PREFIX}-internal-probe"

cleanup() {
  docker rm -f "$PROBE" "$SERVER_A" "$SERVER_B" "$INTERNAL_PROBE" >/dev/null 2>&1 || true
  docker network rm "$NET_A" "$NET_B" "$NET_INTERNAL" "$NET_LOCKED" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

echo "[1/13] creating isolated Docker networks"
docker network create --driver overlay --attachable "$NET_A" >/dev/null
docker network create --driver overlay --attachable "$NET_B" >/dev/null
docker network create --driver overlay --attachable --internal "$NET_INTERNAL" >/dev/null
docker network create --driver overlay "$NET_LOCKED" >/dev/null

echo "[2/13] rejecting unsupported secondary-network drivers"
if docker network create --driver macvlan "${TEST_PREFIX}-macvlan" >/dev/null 2>&1; then
  echo "ERROR: unsupported macvlan network was accepted" >&2
  docker network rm "${TEST_PREFIX}-macvlan" >/dev/null 2>&1 || true
  exit 1
fi
if docker network create --driver ipvlan "${TEST_PREFIX}-ipvlan" >/dev/null 2>&1; then
  echo "ERROR: unsupported ipvlan network was accepted" >&2
  docker network rm "${TEST_PREFIX}-ipvlan" >/dev/null 2>&1 || true
  exit 1
fi

echo "[3/13] rejecting manual attachment to non-attachable overlay"
if docker run --rm --name "${TEST_PREFIX}-locked-probe" --network "$NET_LOCKED" busybox:1.37 true >/dev/null 2>&1; then
  echo "ERROR: standalone workload joined a non-attachable overlay" >&2
  exit 1
fi

echo "[4/13] rejecting host-network escape hatch"
if docker run --rm --name "${TEST_PREFIX}-host-probe" --network host busybox:1.37 true >/dev/null 2>&1; then
  echo "ERROR: host-network workload was accepted while rejection is expected" >&2
  exit 1
fi

echo "[5/13] starting endpoints on separate networks"
docker run -d --name "$SERVER_A" --network "$NET_A" busybox:1.37 httpd -f -p 8080 >/dev/null
docker run -d --name "$SERVER_B" --network "$NET_B" busybox:1.37 httpd -f -p 8080 >/dev/null
docker run -d --name "$PROBE" --network "$NET_A" busybox:1.37 sleep 600 >/dev/null

echo "[6/13] verifying same-network connectivity"
docker exec "$PROBE" wget -q -T 5 -O /dev/null "http://$SERVER_A:8080"

echo "[7/13] verifying disjoint-network isolation"
if docker exec "$PROBE" wget -q -T 3 -O /dev/null "http://$SERVER_B:8080"; then
  echo "ERROR: cross-network traffic unexpectedly succeeded" >&2
  exit 1
fi

echo "[8/13] verifying additive multi-network membership"
docker network connect "$NET_B" "$PROBE"
docker exec "$PROBE" wget -q -T 5 -O /dev/null "http://$SERVER_B:8080"
docker network disconnect "$NET_B" "$PROBE"
if docker exec "$PROBE" wget -q -T 3 -O /dev/null "http://$SERVER_B:8080"; then
  echo "ERROR: traffic still succeeds after network disconnect" >&2
  exit 1
fi

echo "[9/13] verifying explicit full disconnect remains disconnected"
docker network disconnect "$NET_A" "$PROBE"
network_count="$(docker inspect -f '{{len .NetworkSettings.Networks}}' "$PROBE")"
if [ "$network_count" != "0" ]; then
  echo "ERROR: disconnected workload was silently reattached; network count=$network_count" >&2
  exit 1
fi
docker network connect "$NET_A" "$PROBE"
docker exec "$PROBE" wget -q -T 5 -O /dev/null "http://$SERVER_A:8080"

echo "[10/13] verifying in-use network removal is rejected"
if docker network rm "$NET_A" >/dev/null 2>&1; then
  echo "ERROR: in-use Docker network was removed" >&2
  exit 1
fi

echo "[11/13] verifying normal network retains world egress"
docker exec "$PROBE" wget -q -T 5 -O /dev/null http://1.1.1.1

echo "[12/13] verifying internal network blocks ordinary world egress"
docker run -d --name "$INTERNAL_PROBE" --network "$NET_INTERNAL" busybox:1.37 sleep 600 >/dev/null
if docker exec "$INTERNAL_PROBE" wget -q -T 3 -O /dev/null http://1.1.1.1; then
  echo "ERROR: internal network unexpectedly reached a world address" >&2
  exit 1
fi

echo "[13/13] verifying Docker API readback"
docker inspect "$PROBE" >/dev/null
docker network inspect "$NET_A" >/dev/null
docker network inspect "$NET_B" >/dev/null
docker network inspect "$NET_INTERNAL" >/dev/null

echo "PASS: Docker-network isolation behavior validated through the d2k API"
