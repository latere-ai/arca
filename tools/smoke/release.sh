#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# Asserts a running installation from the outside. The release pipeline runs
# it after a rollout and attaches its markdown evidence to the release; an
# operator runs it by hand against their own installation after an upgrade,
# which is why it is a shell script and not a Go test.
#
# It takes its target and has none built in: a smoke with a default origin
# is one that can be run against somebody else's installation by accident.
#
# Required tools: curl, grep.
#
# Environment:
#   BASE_URL   the origin to smoke; required
#   TAG        the release being proved; with it set, the served version
#              must equal it, which is what fails a rollout that returned
#              while replicas of the old version were still answering
#   COMMIT     the release commit, for the evidence
#   DEPLOY_URL the run that deployed it, for the evidence
#   OUTPUT_MD  where to write the markdown evidence; nowhere when unset

set -euo pipefail

BASE_URL="${BASE_URL:-}"
BASE_URL="${BASE_URL%/}"
TAG="${TAG:-}"
COMMIT="${COMMIT:-unknown}"
DEPLOY_URL="${DEPLOY_URL:-}"
OUTPUT_MD="${OUTPUT_MD:-}"

pass() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -n "$BASE_URL" ] || fail "BASE_URL is required; this smoke has no default target"
for cmd in curl grep; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --retry rides out the transient 5xx a load balancer emits while a rollout
# cuts over, so the smoke proves the installation and not the moment.
check_status() {
  local label="$1" path="$2" want="$3" out="$4" code
  code=$(curl -sS --retry 5 --retry-delay 2 --retry-max-time 30 \
    -o "$out" -w '%{http_code}' "${BASE_URL}${path}") || fail "$label: curl failed"
  [ "$code" = "$want" ] || fail "$label: expected $want, got $code (body: $(head -c 200 "$out" 2>/dev/null || true))"
  pass "$label ($code)"
}

check_status "GET /livez" "/livez" "200" "$tmp/livez"
grep -qx ok "$tmp/livez" || fail "GET /livez: the body is not ok"

check_status "GET /readyz" "/readyz" "200" "$tmp/readyz"

# The API description is served from an embed. A build whose embed broke
# answers everything else and fails here, which is the point of the check.
check_status "GET /openapi.json" "/openapi.json" "200" "$tmp/openapi"

check_status "GET /version" "/version" "200" "$tmp/version"
# `|| true` because grep exits 1 when it matches nothing, and under
# pipefail that would end the script before the message below is written. A
# build whose /version answers a page is a real failure and deserves to say
# so rather than to stop silently.
served=$(grep -o '"version":"[^"]*"' "$tmp/version" | head -1 | cut -d'"' -f4 || true)
[ -n "$served" ] || fail "GET /version: no version in $(head -c 200 "$tmp/version")"
if [ -n "$TAG" ]; then
  [ "$served" = "$TAG" ] || fail "version mismatch: served $served, expected $TAG"
  pass "served version matches the tag ($served)"
else
  pass "served version recorded ($served)"
fi

if [ -n "$OUTPUT_MD" ]; then
  {
    echo "<!-- release-evidence -->"
    echo
    echo "## Release evidence"
    echo
    echo "- Tag: \`${TAG:-unknown}\`"
    echo "- Commit: \`${COMMIT}\`"
    [ -n "$DEPLOY_URL" ] && echo "- Deploy: ${DEPLOY_URL}"
    echo "- Origin: ${BASE_URL}"
    echo "- Served version: \`${served}\`"
    echo "- Smoke: \`/livez\`, \`/readyz\`, \`/openapi.json\` and \`/version\` each answered 200"
  } > "$OUTPUT_MD"
fi

printf '\nrelease smoke passed\n'
