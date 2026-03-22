#!/usr/bin/env bash
# Cove installer
# Usage: curl -fsSL https://raw.githubusercontent.com/YOUR_GITHUB_USERNAME/cove/main/install.sh | bash
#
# What this does:
#   1. Detects your Pi architecture (arm64 / arm32)
#   2. Downloads the right Cove binary from GitHub releases
#   3. Installs web files
#   4. Creates a systemd service that starts on boot
#   5. Optionally installs ffmpeg for video faststart (recommended)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
REPO="YOUR_GITHUB_USERNAME/cove"
INSTALL_DIR="/opt/cove"
SERVICE_NAME="cove"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
DEFAULT_STORAGE="/mnt/nas"
DEFAULT_PORT="8080"

# ── Colors ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${BLUE}▸${RESET} $*"; }
success() { echo -e "${GREEN}✓${RESET} $*"; }
warn()    { echo -e "${YELLOW}⚠${RESET}  $*"; }
error()   { echo -e "${RED}✗${RESET} $*" >&2; exit 1; }
header()  { echo -e "\n${BOLD}$*${RESET}"; }

# ── Root check ────────────────────────────────────────────────────────────────
if [[ $EUID -ne 0 ]]; then
  error "Please run as root: sudo bash install.sh"
fi

# ── Welcome ───────────────────────────────────────────────────────────────────
echo -e "${BOLD}"
echo "  🌊 Cove Installer"
echo "  Your personal cloud for Raspberry Pi"
echo -e "${RESET}"

# ── Detect architecture ───────────────────────────────────────────────────────
header "Detecting system..."
ARCH=$(uname -m)
case "$ARCH" in
  aarch64|arm64) BINARY_ARCH="arm64" ;;
  armv7l|armv6l) BINARY_ARCH="arm32" ;;
  x86_64)        BINARY_ARCH="amd64" ;;
  *)             error "Unsupported architecture: $ARCH. Cove supports arm64, arm32, amd64." ;;
esac
info "Architecture: $ARCH → downloading $BINARY_ARCH binary"

# Detect OS
if [[ ! -f /etc/os-release ]]; then
  error "Cannot detect OS. Cove requires a Linux system."
fi
source /etc/os-release
info "OS: $PRETTY_NAME"

# ── Get latest release version ────────────────────────────────────────────────
header "Fetching latest release..."
LATEST=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
if [[ -z "$LATEST" ]]; then
  error "Could not fetch latest release from GitHub. Check your internet connection."
fi
success "Latest version: $LATEST"

BINARY_URL="https://github.com/${REPO}/releases/download/${LATEST}/cove-linux-${BINARY_ARCH}"
WEB_URL="https://github.com/${REPO}/releases/download/${LATEST}/cove-web.tar.gz"

# ── Configuration prompts ─────────────────────────────────────────────────────
header "Configuration"

# Password
while true; do
  read -rsp "$(echo -e "${BOLD}Password${RESET} for Cove (you'll use this to log in): ")" PASSWORD
  echo
  if [[ ${#PASSWORD} -lt 8 ]]; then
    warn "Password must be at least 8 characters. Try again."
  else
    read -rsp "Confirm password: " PASSWORD2
    echo
    if [[ "$PASSWORD" == "$PASSWORD2" ]]; then
      break
    else
      warn "Passwords don't match. Try again."
    fi
  fi
done
success "Password set"

# Storage root
echo
read -rp "$(echo -e "${BOLD}Storage directory${RESET} [${DEFAULT_STORAGE}]: ")" STORAGE_ROOT
STORAGE_ROOT="${STORAGE_ROOT:-$DEFAULT_STORAGE}"
if [[ ! -d "$STORAGE_ROOT" ]]; then
  warn "Directory $STORAGE_ROOT does not exist."
  read -rp "Create it? [Y/n]: " CREATE_DIR
  if [[ "${CREATE_DIR:-Y}" =~ ^[Yy]$ ]]; then
    mkdir -p "$STORAGE_ROOT"
    success "Created $STORAGE_ROOT"
  else
    error "Storage directory must exist. Aborting."
  fi
fi
success "Storage: $STORAGE_ROOT"

# Port
read -rp "$(echo -e "${BOLD}Port${RESET} [${DEFAULT_PORT}]: ")" PORT
PORT="${PORT:-$DEFAULT_PORT}"
success "Port: $PORT"

# ffmpeg
echo
echo -e "${BOLD}Install ffmpeg?${RESET} (recommended — fixes video buffering by optimizing"
echo "  uploaded videos for instant playback. Adds ~200MB.)"
read -rp "[Y/n]: " INSTALL_FFMPEG
INSTALL_FFMPEG="${INSTALL_FFMPEG:-Y}"

# ── Install ffmpeg ────────────────────────────────────────────────────────────
if [[ "$INSTALL_FFMPEG" =~ ^[Yy]$ ]]; then
  header "Installing ffmpeg..."
  if command -v apt-get &>/dev/null; then
    apt-get install -y ffmpeg &>/dev/null
    success "ffmpeg installed"
  elif command -v dnf &>/dev/null; then
    dnf install -y ffmpeg &>/dev/null
    success "ffmpeg installed"
  else
    warn "Could not detect package manager. Install ffmpeg manually: sudo apt install ffmpeg"
  fi
fi

# ── Download binary ───────────────────────────────────────────────────────────
header "Downloading Cove $LATEST..."
mkdir -p "$INSTALL_DIR/web"

info "Downloading binary..."
curl -fsSL --progress-bar "$BINARY_URL" -o "$INSTALL_DIR/cove"
chmod +x "$INSTALL_DIR/cove"
success "Binary downloaded"

info "Downloading web files..."
curl -fsSL "$WEB_URL" -o /tmp/cove-web.tar.gz
tar -xzf /tmp/cove-web.tar.gz -C "$INSTALL_DIR/web" --strip-components=1
rm /tmp/cove-web.tar.gz
success "Web files installed"

# ── Generate JWT secret ───────────────────────────────────────────────────────
JWT_SECRET=$(openssl rand -hex 32 2>/dev/null || cat /dev/urandom | tr -dc 'a-f0-9' | head -c 64)

# ── Create config file ────────────────────────────────────────────────────────
cat > "$INSTALL_DIR/cove.env" <<EOF
COVE_PASSWORD=${PASSWORD}
COVE_SECRET=${JWT_SECRET}
COVE_ROOT=${STORAGE_ROOT}
COVE_PORT=${PORT}
EOF
chmod 600 "$INSTALL_DIR/cove.env"
success "Config written to $INSTALL_DIR/cove.env"

# ── Determine run user ────────────────────────────────────────────────────────
# Run as the user who owns the storage directory, or the sudo user, not root
RUN_USER="${SUDO_USER:-$(stat -c '%U' "$STORAGE_ROOT" 2>/dev/null || echo root)}"
if [[ "$RUN_USER" == "root" ]]; then
  RUN_USER="root"
fi
info "Running Cove as user: $RUN_USER"

# ── Create systemd service ────────────────────────────────────────────────────
header "Setting up systemd service..."
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Cove — Personal Cloud
After=network.target
After=mnt-nas.mount

[Service]
Type=simple
User=${RUN_USER}
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=${INSTALL_DIR}/cove.env
ExecStart=${INSTALL_DIR}/cove \\
  --root \${COVE_ROOT} \\
  --addr :\${COVE_PORT} \\
  --password \${COVE_PASSWORD} \\
  --secret \${COVE_SECRET}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

# Give it a moment to start
sleep 2
if systemctl is-active --quiet "$SERVICE_NAME"; then
  success "Cove service started"
else
  warn "Service may not have started. Check: sudo journalctl -u cove -n 20"
fi

# ── Get local IP ──────────────────────────────────────────────────────────────
LOCAL_IP=$(hostname -I | awk '{print $1}')

# ── Done ──────────────────────────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
echo -e "${GREEN}${BOLD}  🌊 Cove is running!${RESET}"
echo -e "${GREEN}${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
echo
echo -e "  Local:    ${BOLD}http://${LOCAL_IP}:${PORT}${RESET}"
echo -e "  Storage:  ${BOLD}${STORAGE_ROOT}${RESET}"
echo -e "  Config:   ${BOLD}${INSTALL_DIR}/cove.env${RESET}"
echo
echo -e "  Useful commands:"
echo -e "    ${BOLD}sudo systemctl status cove${RESET}   — check status"
echo -e "    ${BOLD}sudo journalctl -u cove -f${RESET}   — live logs"
echo -e "    ${BOLD}sudo systemctl restart cove${RESET}  — restart"
echo
echo -e "  To update Cove later:"
echo -e "    ${BOLD}curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh | sudo bash${RESET}"
echo
