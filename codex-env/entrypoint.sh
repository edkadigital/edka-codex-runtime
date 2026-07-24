#!/usr/bin/env bash
set -euo pipefail

CODEX_HOME="${CODEX_HOME:-/home/codex/.codex}"
WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
CODEX_AUTH_MODE="${CODEX_AUTH_MODE:-openai_api_key}"
CODEX_APP_SERVER_ADDR="${CODEX_APP_SERVER_ADDR:-ws://127.0.0.1:4501}"
CODEX_WS_AUTH_MODE="${CODEX_WS_AUTH_MODE:-none}"

case "${CODEX_AUTH_MODE}" in
    subscription | openai_api_key) ;;
    *)
        echo "unsupported CODEX_AUTH_MODE: ${CODEX_AUTH_MODE}" >&2
        exit 1
        ;;
esac

if [[ -n "${CODEX_MODEL:-}" && ! "${CODEX_MODEL}" =~ ^[A-Za-z0-9._:/-]+$ ]]; then
    echo "CODEX_MODEL contains unsupported characters" >&2
    exit 1
fi
if [[ ! "${WORKSPACE_DIR}" =~ ^/[A-Za-z0-9._/-]+$ ]]; then
    echo "WORKSPACE_DIR must be an absolute path without whitespace" >&2
    exit 1
fi
if [[ ! "${CODEX_APP_SERVER_ADDR}" =~ ^ws://(127\.0\.0\.1|\[::1\]):[0-9]+$ ]]; then
    echo "CODEX_APP_SERVER_ADDR must use a loopback WebSocket address" >&2
    exit 1
fi
if [[ "${CODEX_WS_AUTH_MODE}" != "none" ]]; then
    echo "CODEX_WS_AUTH_MODE must be none behind codex-proxy" >&2
    exit 1
fi

# Provider credentials belong exclusively to the proxy sidecar. Remove
# accidentally supplied legacy variables before executing any user-controlled
# command or the Codex app-server.
unset OPENAI_API_KEY OPENROUTER_API_KEY

umask 077
mkdir -p "${CODEX_HOME}" "${CODEX_HOME}/cache" "${CODEX_HOME}/gh" "${CODEX_HOME}/xdg"
export GIT_CONFIG_GLOBAL="${GIT_CONFIG_GLOBAL:-${CODEX_HOME}/gitconfig}"
export GH_CONFIG_DIR="${GH_CONFIG_DIR:-${CODEX_HOME}/gh}"
export XDG_CACHE_HOME="${XDG_CACHE_HOME:-${CODEX_HOME}/cache}"
export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-${CODEX_HOME}/xdg}"

git config --global user.name "${GIT_USER_NAME:-Edka Codex Agent}"
git config --global user.email "${GIT_USER_EMAIL:-codex-agent@users.noreply.github.com}"
git config --global credential.helper edka

config_tmp="${CODEX_HOME}/config.toml.tmp"
{
    echo 'approval_policy = "never"'
    echo 'sandbox_mode = "danger-full-access"'
    if [[ -n "${CODEX_MODEL:-}" ]]; then
        printf 'model = "%s"\n' "${CODEX_MODEL}"
    fi
    if [[ "${CODEX_AUTH_MODE}" == "openai_api_key" ]]; then
        echo 'model_provider = "edka_openai"'
    fi
    echo
    echo '[features]'
    echo 'apps = false'
    echo
    printf '[projects."%s"]\n' "${WORKSPACE_DIR}"
    echo 'trust_level = "trusted"'
    if [[ "${CODEX_AUTH_MODE}" == "openai_api_key" ]]; then
        echo
        echo '[model_providers.edka_openai]'
        echo 'name = "OpenAI"'
        echo 'base_url = "http://127.0.0.1:4502/v1"'
        echo 'wire_api = "responses"'
        echo 'requires_openai_auth = true'
        echo 'supports_websockets = false'
    fi
} >"${config_tmp}"
mv "${config_tmp}" "${CODEX_HOME}/config.toml"

if [[ $# -gt 0 ]]; then
    if [[ "$1" == "clone" ]]; then
        shift
        exec /usr/local/bin/clone.sh "$@"
    fi
    exec "$@"
fi

if [[ "${CODEX_AUTH_MODE}" == "openai_api_key" ]]; then
    # The remote TUI enters onboarding unless the app-server reports an
    # account. Register a constant, non-secret marker while the provider proxy
    # replaces it with the mounted real key on every upstream request.
    printf '%s\n' 'edka-provider-proxy' | codex login --with-api-key >/dev/null
fi

codex_args=(app-server --listen "${CODEX_APP_SERVER_ADDR}")

exec codex "${codex_args[@]}"
