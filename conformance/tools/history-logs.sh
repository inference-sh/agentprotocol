#!/bin/bash
# history-logs.sh OUT [RUN]: download the CI job logs cmd/history reads into OUT
# (OUT/index.tsv and OUT/raw/<owner>_<repo>/<job>.log.gz): every job of the
# Nightly and per-agent workflows of agentprotocol and belt-sh/harness-test,
# pull request runs left out, one run per workflow per day. Logs already in
# OUT are kept, so a rerun fetches only what is new. With RUN, only that
# run of REPO (default inference-sh/agentprotocol): what the History workflow
# fetches after a nightly.
#
# GitHub's API budget (5000 calls an hour) is the account's, shared with
# every other session using gh. The first version of this script spent all
# of it in ten minutes; this one stops while fewer than KEEP calls remain.
set -u
OUT=${1:?usage: history-logs.sh OUT [RUN]}
RUN=${2:-}
KEEP=${KEEP:-2000}
REPOS=${REPOS:-"belt-sh/harness-test inference-sh/agentprotocol"}
WORKFLOWS="Nightly claude codex copilot cursor droid gemini goose grok hermes kilo kimi kiro omp opencode pi qwen"
mkdir -p "$OUT"

# Job logs are full of terminal escape sequences, which newer gh refuses to
# write without --allow-escape-sequences; older gh has no such flag.
LOGFLAGS=()
gh api --help 2>&1 | grep -q -- --allow-escape-sequences && LOGFLAGS=(--allow-escape-sequences)

budget() {
  local rem reset
  while :; do
    read -r rem reset < <(gh api rate_limit -q '.resources.core|"\(.remaining) \(.reset)"' 2>/dev/null)
    [ "${rem:-0}" -gt "$KEEP" ] && return
    sleep $(( ${reset:-$(( $(date +%s) + 60 ))} - $(date +%s) + 5 ))
  done
}

: > "$OUT/runs.tsv"
if [ -n "$RUN" ]; then
  repo=${REPO:-inference-sh/agentprotocol}
  mkdir -p "$OUT/raw/${repo//\//_}"
  gh run view "$RUN" -R "$repo" --json databaseId,createdAt,event,headBranch,headSha,workflowName \
    -q "[\"$repo\",.workflowName,.databaseId,.createdAt,.event,.headBranch,.headSha]|@tsv" > "$OUT/runs.tsv"
  REPOS=""
fi
for repo in $REPOS; do
  mkdir -p "$OUT/raw/${repo//\//_}"
  for wf in $WORKFLOWS; do
    budget
    gh run list -R "$repo" -w "$wf" -L 1000 --json databaseId,createdAt,event,headBranch,headSha \
      -q ".[]|select(.event!=\"pull_request\")|[\"$repo\",\"$wf\",.databaseId,.createdAt,.event,.headBranch,.headSha]|@tsv" >> "$OUT/runs.tsv"
  done
done
# One run per workflow per day: the latest.
sort -t$'\t' -k1,1 -k2,2 -k4,4r "$OUT/runs.tsv" |
  awk -F'\t' '{k=$1 FS $2 FS substr($4,1,10); if (!(k in seen)) {seen[k]=1; print}}' > "$OUT/picked.tsv"

touch "$OUT/index.tsv"
n=0
failed=0
while IFS=$'\t' read -r repo wf run created event branch sha; do
  d=$OUT/raw/${repo//\//_}
  if [ ! -s "$d/$run.jobs" ]; then
    [ $((n++ % 50)) = 0 ] && budget
    gh api "repos/$repo/actions/runs/$run/jobs?per_page=100" --paginate \
      -q '.jobs[]|[.id,.name,.conclusion]|@tsv' > "$d/$run.jobs.tmp" 2>/dev/null &&
      mv "$d/$run.jobs.tmp" "$d/$run.jobs"
  fi
  [ -f "$d/$run.jobs" ] || continue
  while IFS=$'\t' read -r job name concl; do
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$repo" "$wf" "$run" "$created" "$event" "$branch" "$sha" "$job" "$name" "$concl" >> "$OUT/index.tsv"
    case $name in *CodeQL*|*Analyze*|*binaries*|*lint*|*vet*) continue ;; esac
    [ -s "$d/$job.log.gz" ] && continue
    [ $((n++ % 50)) = 0 ] && budget
    # A failed call (expired log, rate limit) leaves nothing behind.
    if gh api "${LOGFLAGS[@]}" "repos/$repo/actions/jobs/$job/logs" > "$d/$job.log" 2> "$OUT/last-error"; then
      gzip -f "$d/$job.log"
    else
      failed=$((failed + 1))
    fi
    rm -f "$d/$job.log"
  done < "$d/$run.jobs"
done < "$OUT/picked.tsv"
sort -u "$OUT/index.tsv" -o "$OUT/index.tsv"
echo "$(wc -l < "$OUT/index.tsv") jobs, $(find "$OUT/raw" -name '*.log.gz' | wc -l) logs in $OUT, $failed downloads failed"
if [ "$failed" -gt 0 ]; then
  echo "last failure: $(head -c 500 "$OUT/last-error")" >&2
  # One run that yields no log at all is a broken fetch, not an expired log.
  [ -n "$RUN" ] && [ -z "$(find "$OUT/raw" -name '*.log.gz' -print -quit)" ] && exit 1
fi
exit 0
