# edka-codex-runtime

The two container images behind Codex environments on [Edka](https://edka.io). An environment is a pod in your own Kubernetes cluster with one GitHub repository checked out. You attach to it from the [Codex CLI](https://github.com/openai/codex) on your machine, and Codex runs its commands in the pod. Setup and usage are in the [Codex environments docs](https://edka.io/docs/agents/codex/).

```
ghcr.io/edkadigital/edka-codex-env:0.160.0-edka.2
ghcr.io/edkadigital/edka-codex-proxy:0.160.0-edka.2
```

## The environment pod

| Container | Image | What it does |
| --- | --- | --- |
| `clone` (init) | `edka-codex-env` | Clones the repository into the workspace volume at a branch, tag, pull request or commit. A finished checkout survives pod restarts. |
| `codex` | `edka-codex-env` | Runs `codex app-server` on `127.0.0.1:4501` with the workspace as a trusted project. |
| `codex-proxy` | `edka-codex-proxy` | Listens on port `4500`, the only port reachable from outside the pod. Checks each connection's token, forwards it to the app-server and supplies the model credential. |

Codex runs with `approval_policy = "never"` and `sandbox_mode = "danger-full-access"`, so the pod is the sandbox. Edka runs every container as UID `1000` with a read-only root filesystem, no Linux capabilities and no service account token. The environment image has Node.js, npm, pnpm, yarn, Git and the GitHub CLI.

## Where each credential lives

| Credential | Where it is | Who can read it |
| --- | --- | --- |
| GitHub App installation token, limited to the environment's repository | `/var/run/edka/github/token` in `clone` and `codex` | Git, `gh` and every command Codex runs. Review environments mount it in `clone` only. |
| OpenAI Project API key | `/var/run/edka/provider/token` in `codex-proxy` | `codex-proxy` only |
| ChatGPT credential with the refresh token | The `<release>-codex-auth` Secret in your cluster. No container mounts it. | Edka, which refreshes it and writes the new tokens back to the Secret |
| ChatGPT access token | Sent by `codex-proxy` to the Codex app-server | The `codex` container, where Codex also runs its commands. The token expires and the proxy fetches a new one. |
| Connection secret | `/var/run/edka/ws/secret` in `codex-proxy` | `codex-proxy`, to check connection tokens |
| Broker token, ChatGPT mode only | `/var/run/edka/broker/token` in `codex-proxy` | `codex-proxy`, to request access tokens from Edka |

The proxy and the Git helpers read each credential file on every use, so a rotated Secret takes effect without a container restart. Edka replaces the GitHub token before it expires.

The `codex` container drops `OPENAI_API_KEY` from its environment at start. The Git credential helper only answers for `https://github.com`.

## Connecting

The proxy accepts a WebSocket on port `4500` with an `Authorization: Bearer <token>` header. The token is a JWT signed with HS256 using the connection secret, with issuer `edka`, the environment ID as audience, and an expiry. The proxy checks it when the connection opens.

Edka issues a token valid for 60 minutes when you select **Reveal Connection** on the environment. The attach command passes it to Codex:

```bash
codex --dangerously-bypass-approvals-and-sandbox --remote wss://<environment-endpoint> --remote-auth-token-env EDKA_CODEX_TOKEN
```

## Model credentials

`CODEX_AUTH_MODE` picks one of two modes.

### ChatGPT subscription (`subscription`)

After the client's `initialize` request, the proxy signs the app-server in with an access token (`account/login/start` with type `chatgptAuthTokens`). When Codex asks for a new one (`account/chatgptAuthTokens/refresh`), the proxy requests it from Edka at `EDKA_CODEX_BROKER_URL`.

Edka reads the ChatGPT credential from the Secret in your cluster. When the access token has less than 5 minutes left, Edka refreshes it with OpenAI and writes the new tokens back to the Secret. The proxy receives the access token only. If Edka is unreachable, the proxy keeps serving a cached token until 30 seconds before it expires and retries with a backoff of up to 30 seconds.

### OpenAI Project API key (`openai_api_key`)

The environment configures Codex with a model provider at `http://127.0.0.1:4502/v1` and signs it in with the placeholder key `edka-provider-proxy`. The proxy listens on that address, accepts only `/v1/responses` and `/v1/models`, puts the real key in place of the placeholder, and forwards the request to `https://api.openai.com/v1`.

## Settings

### `edka-codex-env`

| Name | Value |
| --- | --- |
| `CODEX_AUTH_MODE` | `subscription` or `openai_api_key` |
| `CODEX_MODEL` | Model for new sessions. Default: Codex's default. |
| `CODEX_HOME` | Default: `/home/codex/.codex` |
| `WORKSPACE_DIR` | Default: `/workspace` |
| `GIT_USER_NAME`, `GIT_USER_EMAIL` | Commit identity. Default: `Edka Codex Agent`, `codex-env@noreply.edka.io` |
| `GITHUB_TOKEN_FILE` | Default: `/var/run/edka/github/token` |

The `clone` argument runs the clone step instead of Codex. It reads `GIT_REPOSITORY_URL` and `GIT_REF`. For a pull request review it also reads `GIT_PR_NUMBER`, with `GIT_REF` set to the commit to review, and `GIT_BASE_REF`.

### `edka-codex-proxy`

Each setting is also a flag. `codex-proxy --help` lists them.

| Name | Value |
| --- | --- |
| `CODEX_AUTH_MODE` | `subscription` or `openai_api_key`. Default: `subscription` |
| `EDKA_CODEX_WS_AUDIENCE` | Required. The audience every connection token must carry. |
| `EDKA_CODEX_WS_SHARED_SECRET_FILE` | At least 32 bytes. Default: `/var/run/edka/ws/secret` |
| `EDKA_CODEX_WS_ISSUER` | Default: `edka` |
| `EDKA_CODEX_PROXY_ADDR` | Default: `:4500` |
| `EDKA_CODEX_PROXY_UPSTREAM` | Default: `ws://127.0.0.1:4501` |
| `EDKA_CODEX_BROKER_URL` | Required in `subscription` mode |
| `EDKA_CODEX_BROKER_TOKEN_FILE` | Default: `/var/run/edka/broker/token` |
| `EDKA_CODEX_PROVIDER_TOKEN_FILE` | Default: `/var/run/edka/provider/token` |

## Versions

A version is the Codex release and a revision of this repository, such as `0.160.0-edka.2`. Both images carry the same tag, and a published tag is never replaced. Each [GitHub release](https://github.com/edkadigital/edka-codex-runtime/releases) has a `bundle.json` with the digests of both images.

[`runtime.env`](runtime.env) pins the Codex, Node.js and GitHub CLI versions, plus SHA-256 checksums for the Codex and GitHub CLI downloads. The build fails when a checksum does not match.

A workflow runs every night at 03:00 UTC. When `openai/codex` has a newer stable release, it pins it with fresh checksums, runs CI and publishes `<codex-version>-edka.1`. A failed CI run leaves the `codex/bump-<version>` branch for inspection, and the next night retries. `.github/scripts/bump-codex.sh` runs the same logic locally.

To release a change to this repository, increase the `edka` revision of `RUNTIME_VERSION` in `runtime.env`, merge it to `main`, and push the tag `v<RUNTIME_VERSION>`. The release workflow builds `linux/amd64` and `linux/arm64` on native runners.

## Tests

```bash
codex-env/scripts_test.sh

cd codex-proxy
go test ./...
go vet ./...
```

To build the images locally:

```bash
set -a; source runtime.env; set +a
docker build \
  --build-arg "CODEX_VERSION=${CODEX_VERSION}" \
  --build-arg "NODE_VERSION=${NODE_VERSION}" \
  --build-arg "CODEX_SHA256_AMD64=${CODEX_SHA256_AMD64}" \
  --build-arg "CODEX_SHA256_ARM64=${CODEX_SHA256_ARM64}" \
  --build-arg "GH_VERSION=${GH_VERSION}" \
  --build-arg "GH_SHA256_AMD64=${GH_SHA256_AMD64}" \
  --build-arg "GH_SHA256_ARM64=${GH_SHA256_ARM64}" \
  -t edka-codex-env:dev \
  codex-env
docker build -t edka-codex-proxy:dev codex-proxy
```

## License

[MIT](LICENSE). The environment image also contains the Codex CLI (Apache 2.0), the GitHub CLI (MIT) and Node.js, each under its own license.
