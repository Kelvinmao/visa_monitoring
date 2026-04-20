#!/usr/bin/env bash
set -euo pipefail

# ─────────────────────────────────────────────
# Usage:
#   ./run.sh                     → build only
#   ./run.sh aggressive          → build + run aggressive mode
#   ./run.sh prewarm             → build + run prewarm mode
#   ./run.sh aggressive -workers 80
# ─────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

MODE="${1:-}"
shift || true   # remaining args forwarded to the binary

BIN_DIR="$SCRIPT_DIR/bin"
CONFIG="$SCRIPT_DIR/config.json"

# ── 1. Check Go ──────────────────────────────
if ! command -v go &>/dev/null; then
  echo "[ERROR] Go is not installed. https://go.dev/dl/"
  exit 1
fi

GO_VER=$(go version | awk '{print $3}' | sed 's/go//')
echo "[BUILD] Go $GO_VER detected"

# ── 2. Build ─────────────────────────────────
mkdir -p "$BIN_DIR"

echo "[BUILD] Building aggressive..."
go build -o "$BIN_DIR/aggressive" ./cmd/aggressive/

echo "[BUILD] Building prewarm..."
go build -o "$BIN_DIR/prewarm" ./cmd/prewarm/

echo "[BUILD] Done → $BIN_DIR/"
ls -lh "$BIN_DIR/"

# ── 3. Run (optional) ────────────────────────
if [[ -z "$MODE" ]]; then
  echo ""
  echo "Build complete. To run:"
  echo "  ./run.sh aggressive          # fire-and-forget burst"
  echo "  ./run.sh prewarm             # pre-warm sessions then burst"
  echo ""
  echo "Both modes read config.json. Override workers with -workers N."
  exit 0
fi

if [[ "$MODE" != "aggressive" && "$MODE" != "prewarm" ]]; then
  echo "[ERROR] Unknown mode '$MODE'. Use 'aggressive' or 'prewarm'."
  exit 1
fi

if [[ ! -f "$CONFIG" ]]; then
  echo "[ERROR] config.json not found at $CONFIG"
  exit 1
fi

echo ""
echo "[RUN] Mode: $MODE"
echo "[RUN] Config: $CONFIG"
echo "[RUN] Extra args: $*"
echo "──────────────────────────────────────────"

# Log file captures both build output above and Go program output below.
# Go's log package writes to stderr, so we merge stderr→stdout.
# Using exec replaces this shell with the Go binary (no zombie process).
LOG_FILE="$SCRIPT_DIR/${MODE}_$(date +%Y%m%d_%H%M%S).log"
echo "[RUN] Logging to: $LOG_FILE"

# Redirect stdout+stderr to both terminal and log file
exec "$BIN_DIR/$MODE" -config "$CONFIG" "$@" 2>&1 | tee -a "$LOG_FILE"
