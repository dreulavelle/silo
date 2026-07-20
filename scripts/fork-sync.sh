#!/usr/bin/env bash
#
# fork-sync.sh — keep this fork rebasable against a fast-moving upstream.
#
# Upstream (Silo-Server/silo-server) publishes Docker images, not git tags, so
# there is no release train to follow. Instead this fork pins an explicit
# upstream commit (fork/upstream.pin) and advances it deliberately.
#
# Modes:
#   check    Report drift and PREDICT conflicts. Read-only, never mutates the
#            working tree. This is what CI runs nightly.
#   rebase   Attempt the rebase in a throwaway worktree, build, and test. Still
#            does not touch your working tree or move any branch; it tells you
#            whether advancing the pin would be safe.
#   advance  Perform the rebase for real on the current branch and rewrite
#            fork/upstream.pin. Requires a clean tree. Humans only.
#
# Exit codes:
#   0  clean — no drift, or drift that rebases and tests cleanly
#   1  usage / environment error
#   2  drift detected that needs human attention (predicted or real conflict,
#      or tests failing after rebase)
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PIN_FILE="fork/upstream.pin"
TOUCHED_FILE="fork/touched-files.txt"
MODE="${1:-check}"

die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
info() { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarn:\033[0m %s\n' "$*"; }
ok() { printf '\033[32mok:\033[0m %s\n' "$*"; }

[[ -f "$PIN_FILE" ]] || die "missing $PIN_FILE (run from a fork checkout)"

# shellcheck disable=SC1090
UPSTREAM_REMOTE=""; UPSTREAM_REF=""; UPSTREAM_COMMIT=""
while IFS='=' read -r key value; do
  [[ "$key" =~ ^[[:space:]]*# ]] && continue
  [[ -z "${key// }" ]] && continue
  case "$key" in
    UPSTREAM_REMOTE) UPSTREAM_REMOTE="$value" ;;
    UPSTREAM_REF)    UPSTREAM_REF="$value" ;;
    UPSTREAM_COMMIT) UPSTREAM_COMMIT="$value" ;;
  esac
done < "$PIN_FILE"

[[ -n "$UPSTREAM_REMOTE" && -n "$UPSTREAM_REF" && -n "$UPSTREAM_COMMIT" ]] \
  || die "$PIN_FILE is missing UPSTREAM_REMOTE / UPSTREAM_REF / UPSTREAM_COMMIT"

git remote get-url "$UPSTREAM_REMOTE" >/dev/null 2>&1 \
  || die "remote '$UPSTREAM_REMOTE' not configured; add it with: git remote add $UPSTREAM_REMOTE https://github.com/Silo-Server/silo-server"

info "fetching $UPSTREAM_REMOTE/$UPSTREAM_REF"
git fetch --quiet "$UPSTREAM_REMOTE" "$UPSTREAM_REF"
UPSTREAM_HEAD="$(git rev-parse "$UPSTREAM_REMOTE/$UPSTREAM_REF")"

if [[ "$UPSTREAM_HEAD" == "$UPSTREAM_COMMIT" ]]; then
  ok "pin is current ($(git rev-parse --short "$UPSTREAM_COMMIT")); no upstream drift"
  exit 0
fi

BEHIND="$(git rev-list --count "$UPSTREAM_COMMIT..$UPSTREAM_HEAD")"
info "drift: $BEHIND upstream commit(s) since pin"
printf '    pinned : %s (%s)\n' "$(git rev-parse --short "$UPSTREAM_COMMIT")" "$(git log -1 --format=%cs "$UPSTREAM_COMMIT")"
printf '    latest : %s (%s)\n' "$(git rev-parse --short "$UPSTREAM_HEAD")" "$(git log -1 --format=%cs "$UPSTREAM_HEAD")"

# --- conflict prediction -----------------------------------------------------
# Cross-reference the files upstream touched since the pin against the small set
# of upstream files this fork edits. A hit is not a guaranteed conflict, but it
# is the only early warning that costs nothing.
COLLISIONS=()
if [[ -f "$TOUCHED_FILE" ]]; then
  UPSTREAM_CHANGED="$(git diff --name-only "$UPSTREAM_COMMIT..$UPSTREAM_HEAD")"
  while IFS= read -r tracked; do
    [[ "$tracked" =~ ^[[:space:]]*# ]] && continue
    [[ -z "${tracked// }" ]] && continue
    if grep -Fxq "$tracked" <<<"$UPSTREAM_CHANGED"; then
      COLLISIONS+=("$tracked")
    fi
  done < "$TOUCHED_FILE"
fi

if (( ${#COLLISIONS[@]} > 0 )); then
  warn "upstream touched ${#COLLISIONS[@]} file(s) this fork also edits:"
  for f in "${COLLISIONS[@]}"; do
    printf '    %s  (%s upstream commit(s))\n' \
      "$f" "$(git rev-list --count "$UPSTREAM_COMMIT..$UPSTREAM_HEAD" -- "$f")"
  done
else
  ok "no overlap between upstream changes and fork-touched files"
fi

if [[ "$MODE" == "check" ]]; then
  (( ${#COLLISIONS[@]} > 0 )) && exit 2
  exit 0
fi

# --- trial rebase in a throwaway worktree ------------------------------------
[[ "$MODE" == "rebase" || "$MODE" == "advance" ]] || die "unknown mode '$MODE' (want: check|rebase|advance)"

CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
TRIAL_DIR="$(mktemp -d -t silo-fork-rebase-XXXXXX)"
TRIAL_BRANCH="fork-sync-trial-$$"
cleanup() {
  git worktree remove --force "$TRIAL_DIR" >/dev/null 2>&1 || true
  git branch -D "$TRIAL_BRANCH" >/dev/null 2>&1 || true
}
trap cleanup EXIT

info "trial rebase of '$CURRENT_BRANCH' onto $(git rev-parse --short "$UPSTREAM_HEAD") in a scratch worktree"
git worktree add --quiet --detach "$TRIAL_DIR" "$CURRENT_BRANCH"
git -C "$TRIAL_DIR" checkout --quiet -b "$TRIAL_BRANCH"

REBASE_OK=1
if ! git -C "$TRIAL_DIR" rebase --quiet "$UPSTREAM_HEAD" >/dev/null 2>&1; then
  REBASE_OK=0
  warn "rebase hit conflicts in:"
  git -C "$TRIAL_DIR" diff --name-only --diff-filter=U | sed 's/^/    /'
  git -C "$TRIAL_DIR" rebase --abort >/dev/null 2>&1 || true
fi

if (( REBASE_OK )); then
  ok "rebase applies cleanly"

  # The Go build embeds web/dist. Building the real frontend costs minutes and
  # proves nothing about a Go-side rebase, so stub it. web/dist is gitignored.
  mkdir -p "$TRIAL_DIR/web/dist"
  printf '<!doctype html><title>fork-sync stub</title>\n' > "$TRIAL_DIR/web/dist/index.html"

  info "running go build"
  if ! (cd "$TRIAL_DIR" && go build ./... >/dev/null 2>&1); then
    warn "go build FAILED after rebase"
    (cd "$TRIAL_DIR" && go build ./... 2>&1 | head -30 | sed 's/^/    /') || true
    REBASE_OK=0
  else
    ok "go build passes"
    info "running go test (this takes a while)"
    if ! (cd "$TRIAL_DIR" && go test ./... >/tmp/fork-sync-test.log 2>&1); then
      warn "go test FAILED after rebase; failing packages:"
      grep -E '^(FAIL|--- FAIL)' /tmp/fork-sync-test.log | head -30 | sed 's/^/    /' || true
      warn "full log: /tmp/fork-sync-test.log"
      REBASE_OK=0
    else
      ok "go test passes"
    fi
  fi
fi

if (( ! REBASE_OK )); then
  warn "advancing the pin is NOT safe right now — resolve the above first"
  exit 2
fi

if [[ "$MODE" == "rebase" ]]; then
  ok "trial clean: advancing the pin is safe. Run '$0 advance' to do it for real."
  exit 0
fi

# --- advance for real --------------------------------------------------------
[[ -z "$(git status --porcelain)" ]] || die "working tree is dirty; commit or stash before 'advance'"

info "rebasing '$CURRENT_BRANCH' onto $(git rev-parse --short "$UPSTREAM_HEAD") for real"
git rebase "$UPSTREAM_HEAD"

NEW_DATE="$(git log -1 --format=%cI "$UPSTREAM_HEAD")"
TODAY="$(date -u +%Y-%m-%d)"
tmp="$(mktemp)"
sed -e "s|^UPSTREAM_COMMIT=.*|UPSTREAM_COMMIT=$UPSTREAM_HEAD|" \
    -e "s|^UPSTREAM_DATE=.*|UPSTREAM_DATE=$NEW_DATE|" \
    -e "s|^PINNED_AT=.*|PINNED_AT=$TODAY|" \
    "$PIN_FILE" > "$tmp"
mv "$tmp" "$PIN_FILE"

git add "$PIN_FILE"
git commit --quiet -m "chore(fork): advance upstream pin to $(git rev-parse --short "$UPSTREAM_HEAD")"
ok "pin advanced to $(git rev-parse --short "$UPSTREAM_HEAD") and committed"
