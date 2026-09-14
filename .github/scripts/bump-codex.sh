#!/usr/bin/env bash
# Pin runtime.env to the newest stable upstream Codex release.
#
# Resolves the newest non-prerelease `rust-v<semver>` tag in openai/codex (or
# CODEX_TARGET_VERSION when set), downloads both Linux musl archives, verifies
# they are well-formed, and rewrites CODEX_VERSION, both Codex checksums, and
# RUNTIME_VERSION (<codex-version>-edka.1) in runtime.env. Never downgrades.
#
# Writes updated/previous_codex_version/codex_version/runtime_version to
# GITHUB_OUTPUT when running inside GitHub Actions.
set -euo pipefail

upstream_repo="openai/codex"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
runtime_env="${RUNTIME_ENV:-${repo_root}/runtime.env}"

emit() {
  echo "$1=$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "$1=$2" >> "${GITHUB_OUTPUT}"
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# shellcheck disable=SC1090
source "${runtime_env}"
if [[ ! "${CODEX_VERSION:-}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::CODEX_VERSION in ${runtime_env} is not a complete semantic version" >&2
  exit 1
fi

if [[ -n "${CODEX_TARGET_VERSION:-}" ]]; then
  target="${CODEX_TARGET_VERSION#rust-v}"
  target="${target#v}"
  if [[ ! "${target}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "::error::CODEX_TARGET_VERSION must be a complete semantic version, got '${CODEX_TARGET_VERSION}'" >&2
    exit 1
  fi
else
  # Upstream publishes many `-alpha` prereleases and separate python-v tags;
  # only stable `rust-v<semver>` releases are candidates.
  target="$(
    gh api "repos/${upstream_repo}/releases?per_page=100" \
      --jq '.[]
        | select(.draft == false and .prerelease == false)
        | .tag_name
        | select(test("^rust-v[0-9]+\\.[0-9]+\\.[0-9]+$"))
        | sub("^rust-v"; "")' |
      sort -V | tail -n 1
  )"
  if [[ -z "${target}" ]]; then
    echo "::error::No stable rust-v release found in ${upstream_repo}" >&2
    exit 1
  fi
fi

echo "Current Codex version: ${CODEX_VERSION}"
echo "Newest stable upstream Codex version: ${target}"

newest="$(printf '%s\n%s\n' "${CODEX_VERSION}" "${target}" | sort -V | tail -n 1)"
if [[ "${target}" == "${CODEX_VERSION}" || "${newest}" != "${target}" ]]; then
  echo "runtime.env already pins Codex ${CODEX_VERSION}; nothing to do"
  emit updated false
  emit previous_codex_version "${CODEX_VERSION}"
  emit codex_version "${CODEX_VERSION}"
  emit runtime_version "${RUNTIME_VERSION}"
  exit 0
fi

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

declare -A checksums
for arch in x86_64 aarch64; do
  archive="codex-package-${arch}-unknown-linux-musl.tar.gz"
  url="https://github.com/${upstream_repo}/releases/download/rust-v${target}/${archive}"
  echo "Downloading ${url}"
  curl --fail --silent --show-error --location --retry 3 --retry-delay 5 \
    --output "${workdir}/${archive}" "${url}"
  # Make sure the archive is a real Codex package before trusting its checksum.
  tar --list --gzip --file "${workdir}/${archive}" | grep -qx 'bin/codex'
  checksums["${arch}"]="$(sha256_of "${workdir}/${archive}")"
  echo "${archive}: ${checksums["${arch}"]}"
done

runtime_version="${target}-edka.1"
awk \
  -v runtime_version="${runtime_version}" \
  -v codex_version="${target}" \
  -v sha_amd64="${checksums[x86_64]}" \
  -v sha_arm64="${checksums[aarch64]}" \
  'BEGIN { FS = OFS = "=" }
   $1 == "RUNTIME_VERSION"    { $2 = runtime_version }
   $1 == "CODEX_VERSION"      { $2 = codex_version }
   $1 == "CODEX_SHA256_AMD64" { $2 = sha_amd64 }
   $1 == "CODEX_SHA256_ARM64" { $2 = sha_arm64 }
   { print }' "${runtime_env}" > "${workdir}/runtime.env"
mv "${workdir}/runtime.env" "${runtime_env}"

echo "Updated ${runtime_env}: Codex ${CODEX_VERSION} -> ${target}, runtime ${runtime_version}"
emit updated true
emit previous_codex_version "${CODEX_VERSION}"
emit codex_version "${target}"
emit runtime_version "${runtime_version}"
