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

# The OpenAI credential belongs exclusively to the proxy sidecar. Remove an
# accidentally supplied value before executing user-controlled commands or the
# Codex app-server.
unset OPENAI_API_KEY

umask 077
mkdir -p "${CODEX_HOME}" "${CODEX_HOME}/cache" "${CODEX_HOME}/gh" "${CODEX_HOME}/xdg"
export GIT_CONFIG_GLOBAL="${GIT_CONFIG_GLOBAL:-${CODEX_HOME}/gitconfig}"
export GH_CONFIG_DIR="${GH_CONFIG_DIR:-${CODEX_HOME}/gh}"
export XDG_CACHE_HOME="${XDG_CACHE_HOME:-${CODEX_HOME}/cache}"
export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-${CODEX_HOME}/xdg}"

# The fallback email must never use users.noreply.github.com: GitHub maps
# those addresses to whichever account owns the local-part username, so an
# unclaimed name hands commit attribution to a squatter. The control plane
# passes the App bot identity; this default stays unattributed.
git config --global user.name "${GIT_USER_NAME:-Edka Codex Agent}"
git config --global user.email "${GIT_USER_EMAIL:-codex-env@noreply.edka.io}"
git config --global credential.helper edka
# The workspace volume root is owned by root (fsGroup only fixes the group),
# so git would refuse the checkout as dubiously owned. Trust exactly the
# managed workspace path.
git config --global safe.directory "${WORKSPACE_DIR}"

# When CODEX_HOME persists inside the workspace checkout, the session
# transcripts it holds must never become committable: shield its top-level
# directory through .git/info/exclude. The exclude file is local-only, lives
# on the same volume, and keeps the directory invisible to `git add -A` and
# plain `git clean -fd`.
case "${CODEX_HOME}" in
    "${WORKSPACE_DIR}"/*)
        if [[ -d "${WORKSPACE_DIR}/.git" && ! -L "${WORKSPACE_DIR}/.git" ]]; then
            codex_home_relative="${CODEX_HOME#"${WORKSPACE_DIR}"/}"
            codex_home_exclude="/${codex_home_relative%%/*}/"
            mkdir -p "${WORKSPACE_DIR}/.git/info"
            if ! grep -qxF -- "${codex_home_exclude}" \
                "${WORKSPACE_DIR}/.git/info/exclude" 2>/dev/null; then
                printf '%s\n' "${codex_home_exclude}" >>"${WORKSPACE_DIR}/.git/info/exclude"
            fi
        fi
        ;;
esac

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
