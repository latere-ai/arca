#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# Deletes the kind stack. Everything the stack holds is in an emptyDir on
# the node, so this takes the bucket and the database with it, which is
# what a disposable stack is for.
#
# Environment:
#   CLUSTER   the kind cluster name, default arca

set -euo pipefail

CLUSTER="${CLUSTER:-arca}"
command -v kind >/dev/null || { echo "kind is required" >&2; exit 1; }

if kind get clusters | grep -qx "$CLUSTER"; then
  kind delete cluster --name "$CLUSTER"
else
  echo "no cluster named $CLUSTER"
fi
