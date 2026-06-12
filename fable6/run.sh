#!/usr/bin/env bash
# fable6/run.sh — execute the fable6 prompt series (metaPrompts backlog) on a
# branch stacked on the fable5 lifecycle PR. Thin wrapper over fable5/run.sh.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

PROMPT_DIR=fable6 \
FABLE5_BRANCH="${FABLE5_BRANCH:-fable6/metaprompts-backlog}" \
FABLE5_PR_BASE="${FABLE5_PR_BASE:-fable5/lifecycle-review-fixes}" \
FABLE5_PR_TITLE="${FABLE5_PR_TITLE:-Backlog: CI tests, observability, matchExpressions, orphan pruning, supply chain, docs}" \
exec fable5/run.sh
