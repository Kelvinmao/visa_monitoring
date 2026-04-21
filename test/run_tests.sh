#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
BIN_DIR="$ROOT_DIR/bin"
SCENARIOS_DIR="$SCRIPT_DIR/scenarios"

MOCK_PORT=9876
MOCK_PID=""
MOCK_LOG="/tmp/mock_server.log"
PREWARM_LOG="/tmp/prewarm_test.log"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

cleanup() {
    if [ -n "$MOCK_PID" ] && kill -0 "$MOCK_PID" 2>/dev/null; then
        kill "$MOCK_PID" 2>/dev/null || true
        wait "$MOCK_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

build() {
    echo -e "${BLUE}[BUILD] Compiling...${NC}"
    cd "$ROOT_DIR"
    go build -o "$BIN_DIR/mockserver" ./cmd/mockserver/
    go build -o "$BIN_DIR/prewarm" ./cmd/prewarm/
    echo -e "${GREEN}[BUILD] Done${NC}"
}

run_scenario() {
    local scenario_file="$1"
    local scenario_name="$(basename "$scenario_file" .json)"
    
    echo -e "\n${BLUE}========================================${NC}"
    echo -e "${BLUE}  SCENARIO: $scenario_name${NC}"
    echo -e "${BLUE}========================================${NC}"
    
    # Timing plan:
    # Mock releases at T+30s
    # Config release = now + 1min (T+60s)
    # start_early_sec = 30
    # Burst fires at T+60s - 30s = T+30s (matches mock release!)
    # Use JST (Asia/Tokyo = UTC+9) since prewarm calculates release in JST
    # Set release to 1 minute from now in JST
    JST_NOW=$(TZ='Asia/Tokyo' date '+%H %M')
    JST_HOUR=$(echo $JST_NOW | awk '{print $1}')
    JST_MIN=$(echo $JST_NOW | awk '{print $2}')
    # Add 1 minute (handle hour rollover)
    NEXT_MIN=$(( (10#$JST_MIN + 1) % 60 ))
    NEXT_HOUR=$(( 10#$JST_HOUR + (10#$JST_MIN + 1) / 60 ))
    if [ $NEXT_HOUR -ge 24 ]; then NEXT_HOUR=0; fi
    RELEASE_HOUR=$NEXT_HOUR
    RELEASE_MIN=$NEXT_MIN
    
    cat > "$ROOT_DIR/config_test.json" << EOF
{
  "target_date": "2026/06/25",
  "event_id": "16",
  "plan_id": "20",
  "family_name": "Mao",
  "first_name": "Kaining",
  "phone": "825-984-7284",
  "email": "kaining@layer6.ai",
  "release_hour": $RELEASE_HOUR,
  "release_minute": $RELEASE_MIN,
  "start_early_sec": 30,
  "burst_duration_min": 2,
  "worker_count": 50,
  "base_url": "http://127.0.0.1:$MOCK_PORT"
}
EOF

    echo -e "${YELLOW}[MOCK] Starting mock server (release in 30s)...${NC}"
    "$BIN_DIR/mockserver" -port $MOCK_PORT -scenario "$scenario_file" -release-delay 30 > "$MOCK_LOG" 2>&1 &
    MOCK_PID=$!
    
    sleep 1
    if ! kill -0 "$MOCK_PID" 2>/dev/null; then
        echo -e "${RED}[ERROR] Mock server failed to start${NC}"
        cat "$MOCK_LOG"
        return 1
    fi
    echo -e "${GREEN}[MOCK] Server ready (PID: $MOCK_PID)${NC}"
    echo -e "${YELLOW}[PREWARM] Starting prewarm...${NC}"
    
    timeout 180 "$BIN_DIR/prewarm" -config "$ROOT_DIR/config_test.json" > "$PREWARM_LOG" 2>&1 || true
    
    echo -e "\n${BLUE}--- Results ---${NC}"
    
    if grep -q "Success=true" "$PREWARM_LOG"; then
        echo -e "${GREEN}[RESULT] BOOKING SUCCEEDED${NC}"
    else
        echo -e "${RED}[RESULT] BOOKING FAILED${NC}"
    fi
    
    echo -e "\n${YELLOW}[KEY LOG LINES]${NC}"
    grep -E "(RESULT|QUICKBURST.*(Requests|Finished|TIMEOUT|All.*full|Starting)|BOOKING SUCCESS)" "$PREWARM_LOG" | head -15 || true
    
    echo -e "\n${YELLOW}[MOCK EVENTS]${NC}"
    grep -E "(COMPETITOR|CANCELLATION|BOOKING SUCCESS|STATS)" "$MOCK_LOG" || true
    
    kill "$MOCK_PID" 2>/dev/null || true
    wait "$MOCK_PID" 2>/dev/null || true
    MOCK_PID=""
}

build

if [ -n "$1" ]; then
    run_scenario "$1"
else
    for scenario in "$SCENARIOS_DIR"/*.json; do
        run_scenario "$scenario"
    done
    
    echo -e "\n${GREEN}========================================${NC}"
    echo -e "${GREEN}  ALL SCENARIOS COMPLETE${NC}"
    echo -e "${GREEN}========================================${NC}"
fi
