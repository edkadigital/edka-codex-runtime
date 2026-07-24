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
if grep -Fq '[model_providers.' "${config_file}"; then
    echo "subscription config unexpectedly contains a custom provider" >&2
    exit 1
fi

fake_bin="${test_root}/fake-bin"
mkdir -p "${fake_bin}"
fake_codex="${fake_bin}/codex"
{
    echo '#!/usr/bin/env bash'
    echo 'set -euo pipefail'
    echo 'if [[ -n "${OPENAI_API_KEY+x}" ]]; then'
    echo '    printf "%s\n" leaked >"${CODEX_TEST_ENV_FILE}"'
    echo 'else'
    echo '    printf "%s\n" confined >"${CODEX_TEST_ENV_FILE}"'
    echo 'fi'
    echo 'if [[ "${1:-}" == "login" ]]; then'
    echo '    printf "%s\n" "$@" >"${CODEX_TEST_LOGIN_ARGS_FILE}"'
    echo '    cat >"${CODEX_TEST_LOGIN_STDIN_FILE}"'
    echo '    exit 0'
    echo 'fi'
    echo 'printf "%s\n" "$@" >"${CODEX_TEST_ARGS_FILE}"'
} >"${fake_codex}"
chmod 0755 "${fake_codex}"

run_api_mode_test() {
    local auth_mode="$1"
    local model="$2"
    local provider_id="$3"
    local mode_root="${test_root}/${auth_mode}"
    local mode_home="${mode_root}/codex-home"
    local args_file="${mode_root}/args"
    local env_file="${mode_root}/env"
    local login_args_file="${mode_root}/login-args"
    local login_stdin_file="${mode_root}/login-stdin"
    mkdir -p "${mode_root}"

    PATH="${fake_bin}:${PATH}" \
        CODEX_HOME="${mode_home}" \
        GIT_CONFIG_GLOBAL="${mode_root}/gitconfig" \
        WORKSPACE_DIR="/workspace" \
        CODEX_AUTH_MODE="${auth_mode}" \
        CODEX_MODEL="${model}" \
        CODEX_WS_AUTH_MODE="none" \
        CODEX_TEST_ARGS_FILE="${args_file}" \
        CODEX_TEST_ENV_FILE="${env_file}" \
        CODEX_TEST_LOGIN_ARGS_FILE="${login_args_file}" \
        CODEX_TEST_LOGIN_STDIN_FILE="${login_stdin_file}" \
        OPENAI_API_KEY="must-not-reach-codex-openai" \
        "${script_dir}/entrypoint.sh"

    local mode_config="${mode_home}/config.toml"
    grep -Fqx "model = \"${model}\"" "${mode_config}"
    grep -Fqx "model_provider = \"${provider_id}\"" "${mode_config}"
    grep -Fqx "[model_providers.${provider_id}]" "${mode_config}"
    grep -Fqx 'name = "OpenAI"' "${mode_config}"
    grep -Fqx 'base_url = "http://127.0.0.1:4502/v1"' "${mode_config}"
    grep -Fqx 'wire_api = "responses"' "${mode_config}"
    grep -Fqx 'requires_openai_auth = true' "${mode_config}"
    grep -Fqx 'supports_websockets = false' "${mode_config}"
    if grep -Fq "[model_providers.${provider_id}.auth]" "${mode_config}"; then
        echo "${auth_mode} config unexpectedly contains provider auth commands" >&2
        exit 1
    fi
    if grep -Eq 'env_key|must-not-reach-codex|OPENAI_API_KEY' "${mode_config}"; then
        echo "${auth_mode} config contains a real-key source" >&2
        exit 1
    fi
    [[ "$(<"${env_file}")" == "confined" ]]
    [[ "$(<"${login_args_file}")" == $'login\n--with-api-key' ]]
    [[ "$(<"${login_stdin_file}")" == "edka-provider-proxy" ]]
    [[ "$(<"${args_file}")" == $'app-server\n--listen\nws://127.0.0.1:4501' ]]
}

run_api_mode_test \
    "openai_api_key" \
    "gpt-5.6-sol" \
    "edka_openai"

if CODEX_HOME="${test_root}/unsupported-auth-home" \
    GIT_CONFIG_GLOBAL="${test_root}/unsupported-auth-gitconfig" \
    WORKSPACE_DIR="/workspace" \
    CODEX_AUTH_MODE="unsupported" \
    "${script_dir}/entrypoint.sh" true 2>/dev/null; then
    echo "entrypoint accepted an unsupported authentication mode" >&2
    exit 1
fi

if CODEX_HOME="${test_root}/invalid-ws-auth-home" \
    GIT_CONFIG_GLOBAL="${test_root}/invalid-ws-auth-gitconfig" \
    WORKSPACE_DIR="/workspace" \
    CODEX_AUTH_MODE="subscription" \
    CODEX_WS_AUTH_MODE="signed-bearer-token" \
    "${script_dir}/entrypoint.sh" true 2>/dev/null; then
    echo "entrypoint accepted direct app-server authentication" >&2
    exit 1
fi

if CODEX_HOME="${test_root}/external-listener-home" \
    GIT_CONFIG_GLOBAL="${test_root}/external-listener-gitconfig" \
    WORKSPACE_DIR="/workspace" \
    CODEX_AUTH_MODE="subscription" \
    CODEX_APP_SERVER_ADDR="ws://0.0.0.0:4501" \
    "${script_dir}/entrypoint.sh" true 2>/dev/null; then
    echo "entrypoint accepted a non-loopback app-server address" >&2
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
