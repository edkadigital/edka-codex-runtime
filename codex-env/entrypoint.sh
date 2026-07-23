#!/usr/bin/env bash
set -euo pipefail

CODEX_HOME="${CODEX_HOME:-/home/codex/.codex}"
WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
CODEX_AUTH_MODE="${CODEX_AUTH_MODE:-openai_api_key}"
CODEX_APP_SERVER_ADDR="${CODEX_APP_SERVER_ADDR:-ws://0.0.0.0:4500}"
CODEX_WS_AUTH_MODE="${CODEX_WS_AUTH_MODE:-}"
CODEX_WS_SHARED_SECRET_FILE="${CODEX_WS_SHARED_SECRET_FILE:-/var/run/edka/ws/secret}"
CODEX_WS_ISSUER="${CODEX_WS_ISSUER:-edka}"
CODEX_WS_AUDIENCE="${CODEX_WS_AUDIENCE:-}"

case "${CODEX_AUTH_MODE}" in
    subscription | openai_api_key | openrouter_api_key) ;;
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

if [[ -z "${CODEX_WS_AUTH_MODE}" ]]; then
    if [[ "${CODEX_AUTH_MODE}" == "subscription" ]]; then
        CODEX_WS_AUTH_MODE="none"
    else
        CODEX_WS_AUTH_MODE="signed-bearer-token"
    fi
fi

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
        echo 'model_provider = "openai"'
    elif [[ "${CODEX_AUTH_MODE}" == "openrouter_api_key" ]]; then
        echo 'model_provider = "openrouter"'
    fi
    echo
    echo '[features]'
    echo 'apps = false'
    echo
    printf '[projects."%s"]\n' "${WORKSPACE_DIR}"
    echo 'trust_level = "trusted"'
    if [[ "${CODEX_AUTH_MODE}" == "openrouter_api_key" ]]; then
        echo
        echo '[model_providers.openrouter]'
        echo 'name = "OpenRouter"'
        echo 'base_url = "https://openrouter.ai/api/v1"'
        echo 'env_key = "OPENROUTER_API_KEY"'
        echo 'wire_api = "responses"'
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

codex_args=(app-server --listen "${CODEX_APP_SERVER_ADDR}")
case "${CODEX_WS_AUTH_MODE}" in
    none)
        ;;
    signed-bearer-token)
        if [[ ! -s "${CODEX_WS_SHARED_SECRET_FILE}" ]]; then
            echo "Codex WebSocket shared secret file is missing or empty" >&2
            exit 1
        fi
        if [[ -z "${CODEX_WS_AUDIENCE}" ]]; then
            echo "CODEX_WS_AUDIENCE is required for signed bearer authentication" >&2
            exit 1
        fi
        codex_args+=(
            --ws-auth signed-bearer-token
            --ws-shared-secret-file "${CODEX_WS_SHARED_SECRET_FILE}"
            --ws-issuer "${CODEX_WS_ISSUER}"
            --ws-audience "${CODEX_WS_AUDIENCE}"
        )
        ;;
    *)
        echo "unsupported CODEX_WS_AUTH_MODE: ${CODEX_WS_AUTH_MODE}" >&2
        exit 1
        ;;
esac

exec codex "${codex_args[@]}"
