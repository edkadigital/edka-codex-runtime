#!/usr/bin/env bash
# Dispatch a workflow on a ref and wait for that run to finish.
#
# Usage: dispatch-and-wait.sh <workflow-file> <ref> <head-sha>
#
# Pushes and tags made with GITHUB_TOKEN never trigger other workflows, so the
# Update Codex workflow dispatches CI and the release explicitly. gh workflow
# run does not return the run id, so the run is located by ref and head SHA.
# Exits non-zero when the run fails or does not start.
set -euo pipefail

workflow="$1"
ref="$2"
head_sha="$3"

gh workflow run "${workflow}" --ref "${ref}"

run_id=""
for _ in $(seq 1 30); do
  sleep 10
  run_id="$(
    gh run list --workflow "${workflow}" --branch "${ref}" --event workflow_dispatch \
      --json databaseId,headSha \
      --jq ".[] | select(.headSha == \"${head_sha}\") | .databaseId" |
      head -n 1
  )"
  if [[ -n "${run_id}" ]]; then
    break
  fi
done
if [[ -z "${run_id}" ]]; then
  echo "::error::${workflow} run on ${ref} did not start" >&2
  exit 1
fi

run_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY}/actions/runs/${run_id}"
echo "${workflow} run: ${run_url}"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  echo "${workflow} run: ${run_url}" >> "${GITHUB_STEP_SUMMARY}"
fi

gh run watch "${run_id}" --interval 15 --exit-status
