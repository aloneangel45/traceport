#!/usr/bin/env bash
# ==============================================================================
# TracePort - Official Universal Installer (Linux & macOS)
# AI Agent Flight Recorder & Time-Travel Debugger
# Repository: https://github.com/aloneangel45/traceport
# ==============================================================================

set -euo pipefail

REPO="aloneangel45/traceport"
BINARY_NAME="traceport"

# Color support
if [ -t 1 ]; then
    BOLD="\033[1m"
    GREEN="\033[32m"
    BLUE="\033[34m"
    CYAN="\033[36m"
    YELLOW="\033[33m"
    RED="\033[31m"
    RESET="\033[0m"
else
    BOLD=""
    GREEN=""
    BLUE=""
    CYAN=""
    YELLOW=""
    RED=""
    RESET=""
fi

log_info() {
    printf "${BLUE}[INFO]${RESET} %s\n" "$1"
}

log_success() {
    printf "${GREEN}[SUCCESS]${RESET} %s\n" "$1"
}

log_warn() {
    printf "${YELLOW}[WARN]${RESET} %s\n" "$1"
}

log_error() {
    printf "${RED}[ERROR]${RESET} %s\n" "$1" >&2
}

# 1. Detect OS
OS_RAW="$(uname -s)"
case "${OS_RAW}" in
    Linux*)     OS="linux" ;;
    Darwin*)    OS="darwin" ;;
    *)
        log_error "Unsupported Operating System: ${OS_RAW}. TracePort supports Linux and macOS (Darwin)."
        exit 1
        ;;
esac

# 2. Detect Architecture
ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
    x86_64|amd64)    ARCH="amd64" ;;
    arm64|aarch64)   ARCH="arm64" ;;
    *)
        log_error "Unsupported architecture: ${ARCH_RAW}. TracePort supports amd64 and arm64."
        exit 1
        ;;
esac

log_info "Detected target platform: ${BOLD}${OS}/${ARCH}${RESET}"

# 3. Resolve latest release version
log_info "Fetching latest release version from GitHub..."
TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4 || true)

if [ -z "${TAG}" ]; then
    TAG="v1.0.0"
    log_warn "GitHub API rate limit reached or offline; falling back to release ${TAG}"
fi

log_info "Target release: ${BOLD}${TAG}${RESET}"

# 4. Prepare Download URL and Temp Directory
ARCHIVE_NAME="traceport-${OS}-${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG}/${ARCHIVE_NAME}"

TMP_DIR="$(mktemp -d)"
cleanup() {
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

log_info "Downloading ${DOWNLOAD_URL}..."
if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${DOWNLOAD_URL}" -o "${TMP_DIR}/${ARCHIVE_NAME}"
elif command -v wget >/dev/null 2>&1; then
    wget -qO "${TMP_DIR}/${ARCHIVE_NAME}" "${DOWNLOAD_URL}"
else
    log_error "Neither curl nor wget found in PATH. Please install one of them."
    exit 1
fi

# 5. Extract Archive
log_info "Extracting ${ARCHIVE_NAME}..."
tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "${TMP_DIR}"

if [ ! -f "${TMP_DIR}/${BINARY_NAME}" ]; then
    log_error "Extraction failed: ${BINARY_NAME} binary not found in archive."
    exit 1
fi

chmod +x "${TMP_DIR}/${BINARY_NAME}"

# 6. Determine Installation Target Directory
if [ -w "/usr/local/bin" ] || [ "$(id -u)" -eq 0 ]; then
    INSTALL_DIR="/usr/local/bin"
    SUDO=""
    if [ ! -w "/usr/local/bin" ] && command -v sudo >/dev/null 2>&1; then
        SUDO="sudo"
    fi
    ${SUDO} mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
else
    INSTALL_DIR="${HOME}/.local/bin"
    mkdir -p "${INSTALL_DIR}"
    mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
fi

log_success "TracePort installed successfully to ${BOLD}${INSTALL_DIR}/${BINARY_NAME}${RESET}"

# 7. Check PATH
case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        log_warn "${INSTALL_DIR} is not in your current PATH."
        printf "  Add it to your shell configuration:\n"
        printf "    ${BOLD}export PATH=\"%s:\$PATH\"${RESET}\n\n" "${INSTALL_DIR}"
        ;;
esac

# 8. Success Banner
printf "\n"
printf "${CYAN}╔══════════════════════════════════════════════════════════════╗${RESET}\n"
printf "${CYAN}║${RESET}  ${BOLD}🛰️ TracePort flight recorder is ready!${RESET}                     ${CYAN}║${RESET}\n"
printf "${CYAN}╚══════════════════════════════════════════════════════════════╝${RESET}\n"
printf "\n"
printf "  Quick Start:\n"
printf "    ${BOLD}traceport record -- aider --model claude-3-7-sonnet${RESET}  # Record agent run\n"
printf "    ${BOLD}traceport sessions${RESET}                                   # List recorded sessions\n"
printf "    ${BOLD}traceport rewind <session-id> --step 3${RESET}             # Time-travel rollback\n"
printf "\n"
printf "  Documentation & Guides: ${BLUE}https://github.com/${REPO}${RESET}\n\n"
