#!/usr/bin/env bash
# ==============================================================================
# TracePort - Universal Installer (Linux & macOS)
# Repository: https://github.com/aloneangel45/traceport
# ==============================================================================

set -euo pipefail

REPO="aloneangel45/traceport"
BINARY_NAME="traceport"

log_info() {
    printf "[traceport] %s\n" "$1"
}

log_ok() {
    printf "[ok] %s\n" "$1"
}

log_warn() {
    printf "[warn] %s\n" "$1"
}

log_err() {
    printf "[err] %s\n" "$1" >&2
}

compute_sha256() {
    local file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "${file}" | awk '{print tolower($1)}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "${file}" | awk '{print tolower($1)}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "${file}" | awk '{print tolower($NF)}'
    else
        log_err "missing SHA-256 utility (install sha256sum, shasum, or openssl)"
        exit 1
    fi
}

download_file() {
    local url="$1"
    local out="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "${url}" -o "${out}"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "${out}" "${url}"
    else
        log_err "neither curl nor wget found in PATH"
        exit 1
    fi
}

OS_RAW="$(uname -s)"
case "${OS_RAW}" in
    Linux*)     OS="linux" ;;
    Darwin*)    OS="darwin" ;;
    *)
        log_err "unsupported operating system: ${OS_RAW} (supported: linux, darwin)"
        exit 1
        ;;
esac

ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
    x86_64|amd64)    ARCH="amd64" ;;
    arm64|aarch64)   ARCH="arm64" ;;
    *)
        log_err "unsupported architecture: ${ARCH_RAW} (supported: amd64, arm64)"
        exit 1
        ;;
esac

log_info "detected platform: ${OS}/${ARCH}"

TAG=""
if command -v curl >/dev/null 2>&1; then
    TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4 || true)
elif command -v wget >/dev/null 2>&1; then
    TAG=$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4 || true)
fi

if [ -z "${TAG}" ]; then
    TAG="v1.1.0"
    log_warn "GitHub API unavailable; falling back to release ${TAG}"
fi

log_info "target release: ${TAG}"

ARCHIVE_NAME="traceport-${OS}-${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
DOWNLOAD_URL="${BASE_URL}/${ARCHIVE_NAME}"
CHECKSUMS_URL="${BASE_URL}/checksums.txt"

TMP_DIR="$(mktemp -d)"
cleanup() {
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

log_info "downloading ${DOWNLOAD_URL}"
download_file "${DOWNLOAD_URL}" "${TMP_DIR}/${ARCHIVE_NAME}"

log_info "downloading ${CHECKSUMS_URL}"
download_file "${CHECKSUMS_URL}" "${TMP_DIR}/checksums.txt"

EXPECTED_SHA=$(awk -v target="${ARCHIVE_NAME}" '$2 == target || $2 == "*"target {print tolower($1); exit}' "${TMP_DIR}/checksums.txt")
if [ -z "${EXPECTED_SHA}" ]; then
    log_err "archive ${ARCHIVE_NAME} not listed in checksums.txt"
    exit 1
fi

ACTUAL_SHA="$(compute_sha256 "${TMP_DIR}/${ARCHIVE_NAME}")"
if [ "${ACTUAL_SHA}" != "${EXPECTED_SHA}" ]; then
    log_err "SHA-256 checksum mismatch for ${ARCHIVE_NAME}: expected ${EXPECTED_SHA}, got ${ACTUAL_SHA}"
    exit 1
fi
log_ok "verified SHA-256 digest: ${ACTUAL_SHA}"

log_info "extracting ${ARCHIVE_NAME}"
tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "${TMP_DIR}"

EXTRACTED_BIN=""
if [ -f "${TMP_DIR}/${BINARY_NAME}" ]; then
    EXTRACTED_BIN="${TMP_DIR}/${BINARY_NAME}"
elif [ -f "${TMP_DIR}/traceport-${OS}-${ARCH}" ]; then
    EXTRACTED_BIN="${TMP_DIR}/traceport-${OS}-${ARCH}"
else
    log_err "extraction failed: binary not found in archive"
    exit 1
fi

chmod 0755 "${EXTRACTED_BIN}"

if [ -w "/usr/local/bin" ] || [ "$(id -u)" -eq 0 ]; then
    INSTALL_DIR="/usr/local/bin"
    mv "${EXTRACTED_BIN}" "${INSTALL_DIR}/${BINARY_NAME}"
else
    INSTALL_DIR="${HOME}/.local/bin"
    mkdir -p "${INSTALL_DIR}"
    mv "${EXTRACTED_BIN}" "${INSTALL_DIR}/${BINARY_NAME}"
fi

log_ok "installed ${INSTALL_DIR}/${BINARY_NAME}"

case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        log_warn "${INSTALL_DIR} is not in PATH; add: export PATH=\"${INSTALL_DIR}:\$PATH\""
        ;;
esac

printf "\n"
printf "Quick Start:\n"
printf "  traceport --help\n"
printf "  traceport record -- <cmd>\n"
printf "  traceport sessions\n"
printf "  traceport rewind <session_id> --step <N>\n"
printf "\n"
