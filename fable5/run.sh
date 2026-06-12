#!/usr/bin/env bash
# fable5/run.sh — execute the fable5 prompt series one at a time on a branch,
# gating each step on build/tests, committing per prompt, and opening a PR.
#
# Safe to re-run: prompts that already produced a commit on the branch are skipped.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

BRANCH="${FABLE5_BRANCH:-fable5/lifecycle-review-fixes}"
CLAUDE_BIN="${CLAUDE_BIN:-claude}"
LOG_DIR="${FABLE5_LOG_DIR:-$(mktemp -d /tmp/fable5-run.XXXXXX)}"
mkdir -p "$LOG_DIR"

command -v "$CLAUDE_BIN" >/dev/null || { echo "error: claude CLI not found" >&2; exit 1; }
command -v gh >/dev/null || { echo "error: gh CLI not found" >&2; exit 1; }

# ── claude invocation flags ───────────────────────────────────────────────────
CLAUDE_ARGS=(-p --permission-mode acceptEdits)
if [[ -n "${FABLE5_MODEL:-}" ]]; then
  CLAUDE_ARGS+=(--model "$FABLE5_MODEL")
fi
if [[ "${FABLE5_YOLO:-0}" == "1" ]]; then
  CLAUDE_ARGS+=(--dangerously-skip-permissions)
else
  CLAUDE_ARGS+=(--allowedTools \
    "Bash(go:*)" "Bash(gofmt:*)" "Bash(controller-gen:*)" \
    "Bash(cp:*)" "Bash(ls:*)" "Bash(cat:*)" "Bash(diff:*)" \
    "Bash(git diff:*)" "Bash(git status:*)" "Bash(git log:*)" \
    "Bash(grep:*)" "Bash(find:*)")
fi

run_claude() { # $1 = prompt text on stdin-safe string
  # Unset nesting markers so a parent Claude Code session doesn't confuse the child.
  env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT "$CLAUDE_BIN" "${CLAUDE_ARGS[@]}" "$1"
}

# ── verification gate ─────────────────────────────────────────────────────────
gate() {
  local out
  out="$LOG_DIR/gate.log"
  {
    unformatted="$(gofmt -l . 2>&1 | grep -v '^vendor/' || true)"
    if [[ -n "$unformatted" ]]; then
      echo "gofmt: files need formatting:"
      echo "$unformatted"
      return 1
    fi
    go vet ./... && go build ./... && go test ./...
  } >"$out" 2>&1
}

# ── branch setup ──────────────────────────────────────────────────────────────
current_branch="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$current_branch" != "$BRANCH" ]]; then
  if git show-ref --verify --quiet "refs/heads/$BRANCH"; then
    git checkout "$BRANCH"
  else
    git checkout -b "$BRANCH"
  fi
fi

# Refuse to start with unrelated uncommitted changes (metaPrompts/ is the user's
# untracked scratch space and is always left alone).
dirty="$(git status --porcelain | grep -v '^?? metaPrompts/' | grep -v '^?? fable5/' || true)"
if [[ -n "$dirty" ]]; then
  echo "error: working tree has uncommitted changes; commit or stash first:" >&2
  echo "$dirty" >&2
  exit 1
fi

# Commit the prompt series itself if not yet committed.
if [[ -n "$(git status --porcelain -- fable5/)" ]]; then
  git add fable5/
  git commit -m "fable5: add lifecycle-review prompt series and runner"
fi

echo "Logs: $LOG_DIR"

# ── main loop ─────────────────────────────────────────────────────────────────
for prompt_file in fable5/[0-9][0-9]-*.md; do
  name="$(basename "$prompt_file" .md)"

  if git log --oneline --fixed-strings --grep "fable5: apply $name" | grep -q .; then
    echo "── $name: already applied, skipping"
    continue
  fi

  echo "── $name: running claude"
  run_claude "$(cat "$prompt_file")" >"$LOG_DIR/$name.log" 2>&1 || {
    echo "error: claude failed on $name (see $LOG_DIR/$name.log)" >&2
    exit 1
  }

  if ! gate; then
    echo "── $name: verification failed, giving claude one retry"
    cp "$LOG_DIR/gate.log" "$LOG_DIR/$name.gate-fail.log"
    retry_prompt="The previous change in this repository left verification failing.
Fix the failures below. Run gofmt, go vet, go build, and go test until all are clean.
Do not commit. Do not touch metaPrompts/.

Verification output:
$(tail -100 "$LOG_DIR/gate.log")"
    run_claude "$retry_prompt" >"$LOG_DIR/$name.retry.log" 2>&1 || true
    if ! gate; then
      echo "error: $name still failing after retry (see $LOG_DIR/gate.log); aborting" >&2
      exit 1
    fi
  fi

  if [[ -z "$(git status --porcelain | grep -v '^?? metaPrompts/' || true)" ]]; then
    echo "── $name: no changes produced; recording as applied"
    git commit --allow-empty -m "fable5: apply $name (no changes needed)"
    continue
  fi

  git add -A -- ':(exclude)metaPrompts'
  git commit -m "fable5: apply $name" -m "Applied prompt $prompt_file; build/vet/test verified."
  echo "── $name: committed"
done

# ── push + PR ─────────────────────────────────────────────────────────────────
git push -u origin "$BRANCH"

if gh pr view "$BRANCH" >/dev/null 2>&1; then
  echo "PR already exists:"
  gh pr view "$BRANCH" --json url -q .url
  exit 0
fi

pr_body="$LOG_DIR/pr-body.md"
{
  echo "Automated fix series from a node-lifecycle code review, applied prompt-by-prompt"
  echo "(prompt files and runner are included under \`fable5/\`)."
  echo
  echo "## Changes"
  echo
  for prompt_file in fable5/[0-9][0-9]-*.md; do
    title="$(head -1 "$prompt_file" | sed 's/^# *//')"
    echo "- **$(basename "$prompt_file" .md)** — ${title#Prompt [0-9][0-9] — }"
  done
  echo
  echo "Each prompt was gated on \`gofmt\` / \`go vet\` / \`go build\` / \`go test\` and"
  echo "committed individually, so the series can be reviewed commit-by-commit."
} >"$pr_body"

gh pr create \
  --title "Node lifecycle hardening: reboot safety, status convergence, teardown correctness" \
  --body-file "$pr_body"
