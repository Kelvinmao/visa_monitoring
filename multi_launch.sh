#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$SCRIPT_DIR/bin"
LOG_DIR="$SCRIPT_DIR/logs"
mkdir -p "$LOG_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Account configs to run
ACCOUNTS=(
    "config_account_1.json"
    "config_account_2.json"
    "config_account_3.json"
)

PIDS=()

cleanup() {
    echo -e "\n${YELLOW}Stopping all processes...${NC}"
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
    echo -e "${GREEN}All stopped.${NC}"
    exit 0
}

trap cleanup SIGINT SIGTERM

build() {
    echo -e "${BLUE}[BUILD] Compiling prewarm...${NC}"
    cd "$SCRIPT_DIR"
    go build -o "$BIN_DIR/prewarm" ./cmd/prewarm/
    echo -e "${GREEN}[BUILD] Done${NC}"
}

stop_existing() {
    # Kill any existing prewarm processes
    pkill -f "bin/prewarm" 2>/dev/null || true
    sleep 1
}

launch() {
    local config="$1"
    local account_name="${config%.json}"
    local log_file="$LOG_DIR/${account_name}_$(date +%Y%m%d_%H%M%S).log"
    
    echo -e "${BLUE}[LAUNCH] Starting $account_name...${NC}"
    echo -e "  Config: $config"
    echo -e "  Log:    $log_file"
    
    "$BIN_DIR/prewarm" -config "$SCRIPT_DIR/$config" > "$log_file" 2>&1 &
    local pid=$!
    PIDS+=($pid)
    
    echo -e "  PID:    $pid"
    echo ""
}

monitor() {
    echo -e "${BLUE}[MONITOR] Watching for booking success...${NC}"
    echo -e "  Press Ctrl+C to stop all\n"
    
    while true; do
        local found_success=false
        
        for config in "${ACCOUNTS[@]}"; do
            local account_name="${config%.json}"
            # Find the latest log file for this account
            local latest_log=$(ls -t "$LOG_DIR"/${account_name}_*.log 2>/dev/null | head -1)
            
            if [ -n "$latest_log" ] && [ -f "$latest_log" ]; then
                if grep -q "BOOKING SUCCESS\|Success=true" "$latest_log" 2>/dev/null; then
                    echo -e "${GREEN}★ $account_name: BOOKING SUCCESS!${NC}"
                    found_success=true
                    
                    # Show the success details
                    grep -E "(BOOKING SUCCESS|RESULT)" "$latest_log" | tail -3
                fi
            fi
        done
        
        if [ "$found_success" = true ]; then
            echo -e "\n${GREEN}========================================${NC}"
            echo -e "${GREEN}  BOOKING SUCCESSFUL!${NC}"
            echo -e "${GREEN}========================================${NC}"
            break
        fi
        
        sleep 5
    done
}

# Main
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  VISA BOOKING MULTI-ACCOUNT LAUNCHER${NC}"
echo -e "${BLUE}  $(date -u +%Y-%m-%d\ %H:%M:%S\ UTC)${NC}"
echo -e "${BLUE}  JST: $(TZ='Asia/Tokyo' date '+%Y-%m-%d %H:%M:%S')${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

# Validate configs exist
for config in "${ACCOUNTS[@]}"; do
    if [ ! -f "$SCRIPT_DIR/$config" ]; then
        echo -e "${RED}[ERROR] Config not found: $config${NC}"
        exit 1
    fi
done

build
stop_existing

echo ""
echo -e "${YELLOW}Launching ${#ACCOUNTS[@]} accounts...${NC}"
echo ""

for config in "${ACCOUNTS[@]}"; do
    launch "$config"
done

echo -e "${GREEN}All accounts launched. PIDs: ${PIDS[*]}${NC}"
echo ""

monitor
