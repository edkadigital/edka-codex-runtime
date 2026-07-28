#!/usr/bin/env bash
set -euo pipefail

WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
GITHUB_TOKEN_FILE="${GITHUB_TOKEN_FILE:-/var/run/edka/github/token}"
GIT_ASKPASS="${GIT_ASKPASS:-/usr/local/bin/git-askpass-edka}"
READY_MARKER_NAME="edka-workspace-ready"
PROGRESS_MARKER_NAME=".edka-clone-in-progress"
STAGE_MARKER_NAME=".edka-managed-clone-stage"

: "${GIT_REPOSITORY_URL:?GIT_REPOSITORY_URL is required}"
: "${GIT_REF:?GIT_REF is required}"

if [[ ! "${WORKSPACE_DIR}" =~ ^/[A-Za-z0-9._/-]+$ || "${WORKSPACE_DIR}" == "/" ]]; then
    echo "WORKSPACE_DIR must be a specific absolute path without whitespace" >&2
    exit 1
fi
if [[ -L "${WORKSPACE_DIR}" ]]; then
    echo "WORKSPACE_DIR must not be a symbolic link" >&2
    exit 1
fi
if [[ ! -s "${GITHUB_TOKEN_FILE}" ]]; then
    echo "GitHub token file is missing or empty" >&2
    exit 1
fi
if [[ "${GIT_REF}" == -* || "${GIT_REF}" == *$'\n'* ]]; then
    echo "GIT_REF is invalid" >&2
    exit 1
fi

mkdir -p "${WORKSPACE_DIR}"
ready_marker="${WORKSPACE_DIR}/.git/${READY_MARKER_NAME}"
progress_marker="${WORKSPACE_DIR}/${PROGRESS_MARKER_NAME}"

remove_managed_stages() {
    local stage
    while IFS= read -r -d '' stage; do
        if [[ -f "${stage}/${STAGE_MARKER_NAME}" ]]; then
            rm -rf -- "${stage}"
        elif [[ -z "$(find "${stage}" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
            rmdir -- "${stage}"
        fi
    done < <(
        find "${WORKSPACE_DIR}" -mindepth 1 -maxdepth 1 -type d -name '.edka-clone-*' -print0
    )
}

if [[ -f "${ready_marker}" && ! -L "${ready_marker}" ]]; then
    if [[ "$(<"${ready_marker}")" != "1" ]]; then
        echo "workspace completion marker is invalid" >&2
        exit 1
    fi
    if ! git -C "${WORKSPACE_DIR}" rev-parse --verify --quiet HEAD >/dev/null; then
        echo "workspace contains an incomplete Git repository; refusing to start" >&2
        exit 1
    fi
    existing_remote="$(git -C "${WORKSPACE_DIR}" remote get-url origin 2>/dev/null || true)"
    if [[ "${existing_remote%.git}" != "${GIT_REPOSITORY_URL%.git}" ]]; then
        echo "workspace repository does not match GIT_REPOSITORY_URL" >&2
        exit 1
    fi
    rm -f -- "${progress_marker}"
    remove_managed_stages
    echo "workspace already contains the expected Git repository; leaving it unchanged"
    exit 0
fi

if [[ -e "${progress_marker}" ]]; then
    if [[ ! -f "${progress_marker}" || -L "${progress_marker}" ]]; then
        echo "workspace clone recovery marker is invalid" >&2
        exit 1
    fi
    interrupted_stage_name="$(<"${progress_marker}")"
    interrupted_stage="${WORKSPACE_DIR}/${interrupted_stage_name}"
    if [[
        ! "${interrupted_stage_name}" =~ ^\.edka-clone-[A-Za-z0-9._-]+$ ||
        ! -f "${interrupted_stage}/${STAGE_MARKER_NAME}"
    ]]; then
        echo "workspace clone recovery marker does not match a managed stage" >&2
        exit 1
    fi
    find "${WORKSPACE_DIR}" -mindepth 1 -maxdepth 1 ! -name lost+found \
        -exec rm -rf -- {} +
fi

remove_managed_stages

if [[ -d "${WORKSPACE_DIR}/.git" ]]; then
    echo "workspace Git repository has no completion marker; refusing to start" >&2
    exit 1
fi

# Some filesystem-backed PVCs include a root-owned lost+found directory that
# the non-root init container cannot remove. Ignore that single entry while
# still refusing to clone over any user data.
rmdir "${WORKSPACE_DIR}/lost+found" 2>/dev/null || true
if [[ -n "$(find "${WORKSPACE_DIR}" -mindepth 1 -maxdepth 1 ! -name lost+found -print -quit)" ]]; then
    echo "workspace is not empty" >&2
    exit 1
fi

export GIT_ASKPASS
export GIT_TERMINAL_PROMPT=0
export GITHUB_TOKEN_FILE

# Clone below the PVC root first. A failed clone is then safely removable and
# cannot leave a partial /workspace/.git that a restarted init container would
# mistake for a usable checkout.
stage_dir="${WORKSPACE_DIR}/.edka-clone-${RANDOM}-$$"
clone_dir="${stage_dir}/checkout"
cleanup_clone_dir() {
    if [[
        -n "${stage_dir}" &&
        "${stage_dir}" == "${WORKSPACE_DIR}"/.edka-clone-* &&
        -f "${stage_dir}/${STAGE_MARKER_NAME}" &&
        ! -e "${progress_marker}"
    ]]; then
        rm -rf -- "${stage_dir}"
    fi
}
trap cleanup_clone_dir EXIT
mkdir -- "${stage_dir}"
: >"${stage_dir}/${STAGE_MARKER_NAME}"

if [[ -n "${GIT_PR_NUMBER:-}" ]]; then
    # Review checkout: fetch the PR head ref from the base repository. This is
    # fork-safe (the base repo advertises refs/pull/N/head) and pins to the
    # exact SHA the review was requested for: a head that moved since
    # resolution fails loudly instead of silently reviewing newer commits.
    git init "${clone_dir}"
    git -C "${clone_dir}" remote add origin "${GIT_REPOSITORY_URL}"
    git -C "${clone_dir}" fetch --depth=1 origin "refs/pull/${GIT_PR_NUMBER}/head"
    if ! git -C "${clone_dir}" cat-file -e "${GIT_REF}^{commit}" 2>/dev/null; then
        echo "PR #${GIT_PR_NUMBER} head no longer matches the pinned SHA ${GIT_REF}; recreate the review environment." >&2
        exit 1
    fi
    git -C "${clone_dir}" checkout --detach "${GIT_REF}"
elif git ls-remote --exit-code --heads "${GIT_REPOSITORY_URL}" "${GIT_REF}" >/dev/null 2>&1 ||
    git ls-remote --exit-code --tags "${GIT_REPOSITORY_URL}" "${GIT_REF}" >/dev/null 2>&1; then
    # Preserve a normal branch checkout when the requested ref is an advertised
    # branch or tag. This keeps branch-based environments ready to commit/push.
    git clone --depth=1 --single-branch --branch "${GIT_REF}" -- \
        "${GIT_REPOSITORY_URL}" "${clone_dir}"
else
    # Raw commit SHAs and refs such as refs/pull/* are not accepted by
    # `git clone --branch`. Fetch the exact object and intentionally leave the
    # workspace detached so callers cannot mistake it for a named branch.
    git init "${clone_dir}"
    git -C "${clone_dir}" remote add origin "${GIT_REPOSITORY_URL}"
    git -C "${clone_dir}" fetch --depth=1 origin "${GIT_REF}"
    git -C "${clone_dir}" checkout --detach FETCH_HEAD
fi

if [[ -n "${GIT_BASE_REF:-}" ]]; then
    # Materialize the PR's base while credentials still exist (review
    # environments never see the token after this init container). Bounded
    # deepening first; a full unshallow only as the correctness fallback.
    git -C "${clone_dir}" fetch --depth=200 origin "${GIT_BASE_REF}"
    git -C "${clone_dir}" branch -f edka-review-base FETCH_HEAD
    git -C "${clone_dir}" fetch --deepen=200 origin || true
    if ! git -C "${clone_dir}" merge-base HEAD edka-review-base >/dev/null 2>&1; then
        git -C "${clone_dir}" fetch --unshallow origin || true
    fi
    if ! merge_base="$(git -C "${clone_dir}" merge-base HEAD edka-review-base)"; then
        echo "Unable to find a merge base between ${GIT_REF} and ${GIT_BASE_REF}" >&2
        exit 1
    fi
    # Reviewers read this instead of rediscovering the diff boundary.
    {
        printf 'BASE_REF=%s\n' "${GIT_BASE_REF}"
        printf 'BASE_LOCAL_BRANCH=edka-review-base\n'
        printf 'MERGE_BASE=%s\n' "${merge_base}"
    } >"${clone_dir}/.edka-review"
fi

shopt -s dotglob nullglob
cloned_entries=("${clone_dir}"/*)
if [[ ${#cloned_entries[@]} -eq 0 ]]; then
    echo "Git clone completed without producing a checkout" >&2
    exit 1
fi

stage_name="${stage_dir##*/}"
printf '%s\n' "${stage_name}" >"${stage_dir}/${PROGRESS_MARKER_NAME}"
mv -- "${stage_dir}/${PROGRESS_MARKER_NAME}" "${progress_marker}"
mv -- "${cloned_entries[@]}" "${WORKSPACE_DIR}/"
printf '%s\n' "1" >"${stage_dir}/${READY_MARKER_NAME}"
mv -- "${stage_dir}/${READY_MARKER_NAME}" "${ready_marker}"
rm -f -- "${progress_marker}"
rm -rf -- "${stage_dir}"
trap - EXIT
