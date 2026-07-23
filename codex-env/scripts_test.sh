#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "${test_root}"' EXIT

token_file="${test_root}/token"
printf '%s\n' 'repository-token' >"${token_file}"

github_output="$(
    printf 'protocol=https\nhost=github.com\n\n' |
        GITHUB_TOKEN_FILE="${token_file}" "${script_dir}/git-credential-edka" get
)"
[[ "${github_output}" == *'username=x-access-token'* ]]
[[ "${github_output}" == *'password=repository-token'* ]]

other_output="$(
    printf 'protocol=https\nhost=example.com\n\n' |
        GITHUB_TOKEN_FILE="${token_file}" "${script_dir}/git-credential-edka" get
)"
[[ -z "${other_output}" ]]

http_output="$(
    printf 'protocol=http\nhost=github.com\n\n' |
        GITHUB_TOKEN_FILE="${token_file}" "${script_dir}/git-credential-edka" get
)"
[[ -z "${http_output}" ]]

codex_home="${test_root}/codex-home"
git_config="${test_root}/gitconfig"
CODEX_HOME="${codex_home}" \
    GIT_CONFIG_GLOBAL="${git_config}" \
    WORKSPACE_DIR="/workspace" \
    CODEX_AUTH_MODE="subscription" \
    "${script_dir}/entrypoint.sh" true

config_file="${codex_home}/config.toml"
grep -Fqx 'approval_policy = "never"' "${config_file}"
grep -Fqx 'sandbox_mode = "danger-full-access"' "${config_file}"
grep -Fqx '[features]' "${config_file}"
grep -Fqx 'apps = false' "${config_file}"
grep -Fqx '[projects."/workspace"]' "${config_file}"
grep -Fqx 'trust_level = "trusted"' "${config_file}"

# API-key serve path: the entrypoint must log the mode's key into the
# app-server before serving. A fake codex on PATH records the login stdin
# and the final app-server invocation.
fake_bin="${test_root}/bin"
mkdir -p "${fake_bin}"
cat >"${fake_bin}/codex" <<'FAKE'
#!/usr/bin/env bash
if [[ "$1" == "login" ]]; then
    cat >"${FAKE_CODEX_DIR}/login-stdin"
    printf '%s\n' "$*" >"${FAKE_CODEX_DIR}/login-args"
    exit 0
fi
printf '%s\n' "$*" >"${FAKE_CODEX_DIR}/serve-args"
exit 0
FAKE
chmod +x "${fake_bin}/codex"

openrouter_home="${test_root}/openrouter-home"
ws_secret_file="${test_root}/ws-secret"
printf '%s\n' 'ws-shared-secret' >"${ws_secret_file}"
FAKE_CODEX_DIR="${test_root}" \
    PATH="${fake_bin}:${PATH}" \
    CODEX_HOME="${openrouter_home}" \
    GIT_CONFIG_GLOBAL="${test_root}/openrouter-gitconfig" \
    WORKSPACE_DIR="/workspace" \
    CODEX_AUTH_MODE="openrouter_api_key" \
    OPENROUTER_API_KEY="sk-or-v1-test-key" \
    CODEX_WS_SHARED_SECRET_FILE="${ws_secret_file}" \
    CODEX_WS_AUDIENCE="environment-123" \
    "${script_dir}/entrypoint.sh"
grep -Fqx 'model_provider = "openrouter"' "${openrouter_home}/config.toml"
grep -Fqx 'env_key = "OPENROUTER_API_KEY"' "${openrouter_home}/config.toml"
[[ "$(<"${test_root}/login-stdin")" == 'sk-or-v1-test-key' ]]
[[ "$(<"${test_root}/login-args")" == 'login --with-api-key' ]]
serve_args="$(<"${test_root}/serve-args")"
[[ "${serve_args}" == *'app-server'* ]]
[[ "${serve_args}" == *'--ws-auth signed-bearer-token'* ]]
[[ "${serve_args}" == *'--ws-audience environment-123'* ]]

# A missing key must fail closed before the app-server starts.
if FAKE_CODEX_DIR="${test_root}" \
    PATH="${fake_bin}:${PATH}" \
    CODEX_HOME="${test_root}/missing-key-home" \
    GIT_CONFIG_GLOBAL="${test_root}/missing-key-gitconfig" \
    WORKSPACE_DIR="/workspace" \
    CODEX_AUTH_MODE="openai_api_key" \
    "${script_dir}/entrypoint.sh" 2>/dev/null; then
    echo "entrypoint unexpectedly served without an API key" >&2
    exit 1
fi

source_repo="${test_root}/source"
git init --initial-branch=main "${source_repo}" >/dev/null
git -C "${source_repo}" config user.name 'Codex Env Test'
git -C "${source_repo}" config user.email 'codex-env-test@edka.io'
printf '%s\n' '# clone fixture' >"${source_repo}/README.md"
git -C "${source_repo}" add README.md
git -C "${source_repo}" commit -m 'Add clone fixture' >/dev/null
first_commit="$(git -C "${source_repo}" rev-parse HEAD)"
printf '%s\n' 'latest branch content' >"${source_repo}/LATEST.md"
git -C "${source_repo}" add LATEST.md
git -C "${source_repo}" commit -m 'Add latest branch fixture' >/dev/null

run_clone() {
    local git_ref="${2:-main}"
    WORKSPACE_DIR="$1" \
        GITHUB_TOKEN_FILE="${token_file}" \
        GIT_REPOSITORY_URL="file://${source_repo}" \
        GIT_REF="${git_ref}" \
        "${script_dir}/clone.sh" >/dev/null
}

workspace="${test_root}/workspace"
mkdir -p "${workspace}/lost+found"
printf '%s\n' 'filesystem-owned fixture' >"${workspace}/lost+found/fixture"
chmod 000 "${workspace}/lost+found"
run_clone "${workspace}"
[[ -f "${workspace}/README.md" ]]
[[ -d "${workspace}/.git" ]]
[[ "$(<"${workspace}/.git/edka-workspace-ready")" == "1" ]]
[[ -d "${workspace}/lost+found" ]]
run_clone "${workspace}"
chmod 700 "${workspace}/lost+found"

commit_workspace="${test_root}/commit-workspace"
run_clone "${commit_workspace}" "${first_commit}"
[[ "$(git -C "${commit_workspace}" rev-parse HEAD)" == "${first_commit}" ]]
[[ -f "${commit_workspace}/README.md" ]]
[[ ! -e "${commit_workspace}/LATEST.md" ]]
[[ "$(git -C "${commit_workspace}" symbolic-ref --quiet --short HEAD || true)" == '' ]]

interrupted_workspace="${test_root}/interrupted-workspace"
interrupted_stage_name=".edka-clone-interrupted"
mkdir -p \
    "${interrupted_workspace}/${interrupted_stage_name}/checkout" \
    "${interrupted_workspace}/.git"
: >"${interrupted_workspace}/${interrupted_stage_name}/.edka-managed-clone-stage"
printf '%s\n' "${interrupted_stage_name}" \
    >"${interrupted_workspace}/.edka-clone-in-progress"
printf '%s\n' 'partial checkout' >"${interrupted_workspace}/partial-file"
run_clone "${interrupted_workspace}"
[[ -f "${interrupted_workspace}/README.md" ]]
[[ ! -e "${interrupted_workspace}/partial-file" ]]
[[ ! -e "${interrupted_workspace}/${interrupted_stage_name}" ]]

stale_workspace="${test_root}/stale-workspace"
mkdir -p "${stale_workspace}/.edka-clone-stale/checkout"
: >"${stale_workspace}/.edka-clone-stale/.edka-managed-clone-stage"
printf '%s\n' 'partial clone' >"${stale_workspace}/.edka-clone-stale/checkout/file"
run_clone "${stale_workspace}"
[[ -f "${stale_workspace}/README.md" ]]
[[ ! -e "${stale_workspace}/.edka-clone-stale" ]]

occupied_workspace="${test_root}/occupied-workspace"
mkdir -p "${occupied_workspace}"
printf '%s\n' 'user data' >"${occupied_workspace}/keep"
if run_clone "${occupied_workspace}" 2>/dev/null; then
    echo "clone unexpectedly replaced an occupied workspace" >&2
    exit 1
fi
[[ "$(<"${occupied_workspace}/keep")" == "user data" ]]

printf 'codex-env script checks passed\n'
