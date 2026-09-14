# edka-codex-runtime

`edka-codex-runtime` builds the two containers used by Codex environments in Edka-managed
clusters:

- `ghcr.io/edkaio/edka-codex-env` contains the pinned Codex CLI, Node.js runtime, GitHub CLI,
  Git tooling, and the workspace bootstrap scripts.
- `ghcr.io/edkaio/edka-codex-proxy` authenticates remote WebSocket connections and injects either
  short-lived ChatGPT subscription credentials or a mounted OpenAI Project API key.

The images are released together as one compatibility bundle. `RUNTIME_VERSION` in `runtime.env`
is the immutable image tag for both images. The same file pins the upstream Codex, Node.js, and
GitHub CLI versions, plus SHA-256 checksums for the downloaded Codex and GitHub CLI artifacts.

## Runtime contract

Edka creates the Kubernetes resources, mounts repository-scoped GitHub credentials, and supplies
the environment variables and token files consumed by these images. This repository owns only the
runtime images and their compatibility tests.

The runtime supports:

- `openai_api_key` and `subscription` authentication modes;
- signed bearer authentication at the remote WebSocket proxy;
- headless operation with Codex Apps disabled and Node.js available to bundled plugin MCP servers;
- crash-safe branch, tag, pull-request ref, and commit-SHA workspace initialization;
- Git and `gh` credentials read from the mounted token file on every invocation.

It runs as UID/GID `1000`, uses `/workspace` as the repository root, and stores writable runtime
state under `/home/codex` and `/tmp`. Edka mounts the GitHub token into the environment container at
`/var/run/edka/github/token` and the remote-auth secret into the proxy container at
`/var/run/edka/ws/secret`.

Every mode runs the Codex app-server without remote authentication on loopback port `4501`. The
authenticated proxy is the only container listening for remote WebSocket connections on port
`4500`.

Every mode requires:

- `CODEX_AUTH_MODE`
- `EDKA_CODEX_WS_SHARED_SECRET_FILE`
- `EDKA_CODEX_WS_AUDIENCE`

In `subscription` mode, the proxy additionally requires:

- `EDKA_CODEX_BROKER_URL`
- `EDKA_CODEX_BROKER_TOKEN_FILE`

In `openai_api_key` mode, the proxy does not use the subscription broker. It additionally reads
`EDKA_CODEX_PROVIDER_TOKEN_FILE` (default
`/var/run/edka/provider/token`), listens on the fixed loopback address `127.0.0.1:4502`, and
forwards only `/v1/responses` and `/v1/models` paths to the fixed OpenAI upstream:

- `https://api.openai.com/v1`

The OpenAI Project API key is mounted only into the proxy container at
`/var/run/edka/provider/token`. The proxy reads that file for every provider request, replaces a
constant non-secret marker bearer token, and never places the real key in the environment
container's config, environment, arguments, or Codex auth file. This keeps the credential
unavailable to commands executed by Codex and allows key rotation without rebuilding the
environment.

The environment config uses the custom `edka_openai` provider, requires Codex API-key
authentication, and disables provider WebSockets. At startup, the environment records only a
constant marker with `codex login --with-api-key`; that makes the remote TUI skip onboarding while
all model traffic stays on the loopback HTTP proxy.

Optional proxy settings are documented by `codex-proxy --help`.

## Development

```bash
bash -n codex-env/*.sh codex-env/gh codex-env/git-askpass-edka \
  codex-env/git-credential-edka
codex-env/scripts_test.sh

cd codex-proxy
gofmt -d .
go test ./...
go vet ./...
```

Docker validation requires a running daemon:

```bash
set -a
source runtime.env
set +a
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

## Releasing

1. Update `CODEX_VERSION` and the Codex checksums in `runtime.env`, plus compatibility tests, when
   upgrading Codex.
2. Bump `RUNTIME_VERSION`. Wrapper-only changes increment the `edka` revision.
3. Merge the verified change to `main`.
4. Source `runtime.env`, then create and push `v${RUNTIME_VERSION}`.

Codex upgrades are automated. The `Update Codex` workflow runs nightly, pins the newest stable
`rust-v` release from `openai/codex` with fresh checksums as `<codex-version>-edka.1`, pushes the
bump to a `codex/bump-<codex-version>` branch, and runs CI on it. When CI passes it fast-forwards
`main`, pushes the release tag, and starts the release workflow. When CI fails the branch and the
failed run stay behind for inspection, and the next nightly run retries. Run the workflow manually to
pin a specific version or to preview a bump with `dry_run`. The same resolution logic is available
locally:

```bash
.github/scripts/bump-codex.sh
```

The tag workflow builds both architectures on native runners, publishes both immutable bundle
tags, updates `latest`, verifies that both GHCR packages are public, and creates one GitHub release
with a `bundle.json` manifest. Existing immutable tags are never overwritten.

GitHub creates new packages as private. After the first publish, an organization owner must make
`edka-codex-env` and `edka-codex-proxy` public in their package settings. This is a one-time,
irreversible action; rerun the release workflow after both packages are public.
