# syntax=docker/dockerfile:1.7

FROM debian:bookworm-slim

ARG CODEX_VERSION
ARG TARGETARCH
ARG CODEX_SHA256_AMD64
ARG CODEX_SHA256_ARM64
ARG GH_VERSION
ARG GH_SHA256_AMD64
ARG GH_SHA256_ARM64

# Debian security updates intentionally remain available when this image is
# rebuilt; the downloaded Codex and GitHub CLI artifacts are pinned separately.
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        git \
        jq \
        less \
        openssh-client \
    && rm -rf /var/lib/apt/lists/*

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) codex_arch="x86_64"; codex_sha256="${CODEX_SHA256_AMD64}" ;; \
        arm64) codex_arch="aarch64"; codex_sha256="${CODEX_SHA256_ARM64}" ;; \
        *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    codex_archive="codex-package-${codex_arch}-unknown-linux-musl.tar.gz"; \
    curl --fail --location --retry 3 \
        --output "/tmp/${codex_archive}" \
        "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/${codex_archive}"; \
    echo "${codex_sha256}  /tmp/${codex_archive}" | sha256sum --check --strict; \
    mkdir -p /opt/codex; \
    tar --extract --gzip --file "/tmp/${codex_archive}" --directory /opt/codex; \
    ln -s /opt/codex/bin/codex /usr/local/bin/codex; \
    ln -s /opt/codex/codex-path/rg /usr/local/bin/rg; \
    rm "/tmp/${codex_archive}"; \
    codex --version

RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) gh_arch="amd64"; gh_sha256="${GH_SHA256_AMD64}" ;; \
        arm64) gh_arch="arm64"; gh_sha256="${GH_SHA256_ARM64}" ;; \
        *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    gh_archive="gh_${GH_VERSION}_linux_${gh_arch}.tar.gz"; \
    curl --fail --location --retry 3 \
        --output "/tmp/${gh_archive}" \
        "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${gh_archive}"; \
    echo "${gh_sha256}  /tmp/${gh_archive}" | sha256sum --check --strict; \
    tar --extract --gzip --file "/tmp/${gh_archive}" --directory /tmp; \
    install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${gh_arch}/bin/gh" /usr/bin/gh; \
    rm -r "/tmp/gh_${GH_VERSION}_linux_${gh_arch}" "/tmp/${gh_archive}"; \
    /usr/bin/gh --version

RUN useradd --create-home --home-dir /home/codex --shell /bin/bash --uid 1000 codex \
    && install -d -o codex -g codex /home/codex/.codex /workspace

COPY --chmod=0755 entrypoint.sh clone.sh git-askpass-edka git-credential-edka gh /usr/local/bin/

ENV CODEX_HOME=/home/codex/.codex \
    GITHUB_TOKEN_FILE=/var/run/edka/github/token \
    HOME=/home/codex \
    WORKSPACE_DIR=/workspace

USER 1000:1000
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
