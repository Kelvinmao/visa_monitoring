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
CYAN='\033[0;36m'
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

# Run a single test with specific worker count and scenario
# Returns: 0=success, 1=failure
run_single_test() {
    local scenario_file="$1"
    local workers="$2"
    local release_delay="$3"
    
    # Use JST current time + 1 minute for release
    # JST = UTC+9
    JST_NOW=$(TZ='Asia/Tokyo' date '+%H %M')
    JST_HOUR=$(echo $JST_NOW | awk '{print $1}')
    JST_MIN=$(echo $JST_NOW | awk '{print $2}')
    NEXT_MIN=$(( (10#$JST_MIN + 1) % 60 ))
    NEXT_HOUR=$(( 10#$JST_HOUR + (10#$JST_MIN + 1) / 60 ))
    if [ $NEXT_HOUR -ge 24 ]; then NEXT_HOUR=0; fi
    
    # Create test config
    cat > "$ROOT_DIR/config_test.json" << EOF
{
  "target_date": "2026/06/25",
  "event_id": "16",
  "plan_id": "20",
  "family_name": "Mao",
  "first_name": "Kaining",
  "phone": "825-984-7284",
  "email": "kaining@layer6.ai",
  "release_hour": $NEXT_HOUR,
  "release_minute": $NEXT_MIN,
  "start_early_sec": 30,
  "burst_duration_min": 2,
  "worker_count": $workers,
  "base_url": "http://127.0.0.1:$MOCK_PORT"
}
EOF

    # Start mock server with 90s delay (prewarm needs ~75s for 50 workers)
    "$BIN_DIR/mockserver" -port $MOCK_PORT -scenario "$scenario_file" -release-delay 90 > "$MOCK_LOG" 2>&1 &
    MOCK_PID=$!
    
    sleep 1
    if ! kill -0 "$MOCK_PID" 2>/dev/null; then
        return 2 # mock server failed
    fi
    
    # Run prewarm with timeout
    timeout 180 "$BIN_DIR/prewarm" -config "$ROOT_DIR/config_test.json" > "$PREWARM_LOG" 2>&1 || true
    
    local result=1
    if grep -q "Success=true" "$PREWARM_LOG" 2>/dev/null; then
        result=0
    fi
    
    # Cleanup
    kill "$MOCK_PID" 2>/dev/null || true
    wait "$MOCK_PID" 2>/dev/null || true
    MOCK_PID=""
    
    return $result
}

# Get timing stats from prewarm log
get_stats() {
    local log_file="$1"
    local total_reqs=$(grep -oP 'totalRequests=\K\d+' "$log_file" | tail -1 || echo "0")
    local first_req_time=$(grep -oP 't\+\K[\d.]+s' "$log_file" | head -1 || echo "N/A")
    local booking_worker=$(grep -oP 'worker=\K\d+ BOOKING SUCCESS' "$log_file" | head -1 || echo "N/A")
    echo "reqs=$total_reqs,first_req=$first_req_time,worker=$booking_worker"
}

build

# Define test matrix - focused on key scenarios
# Format: scenario_name:worker_count:release_delay:rounds
TESTS=(
    # Baseline: no competition
    "01_easy:50:180:2"
    "01_easy:200:180:2"
    
    # Hard race: 10 competitors
    "02_hard_race:50:180:2"
    "02_hard_race:100:180:2"
    "02_hard_race:200:180:2"
    
    # Extreme: 200 competitors (all slots taken at release)
    "06_extreme_race:50:180:2"
    "06_extreme_race:100:180:2"
    "06_extreme_race:200:180:2"
    
    # Ultra extreme: 500 competitors
    "07_ultra_extreme:50:180:2"
    "07_ultra_extreme:100:180:2"
    "07_ultra_extreme:200:180:2"
    
    # Cancellation: hope after being locked out
    "08_cancellation_hope:50:180:2"
    "08_cancellation_hope:100:180:2"
    "08_cancellation_hope:200:180:2"
)

# Results tracking
RESULTS_FILE="/tmp/test_results.csv"
echo "scenario,workers,round,success,time_stats" > "$RESULTS_FILE"

TOTAL_TESTS=0
TOTAL_SUCCESS=0

echo -e "\n${CYAN}========================================${NC}"
echo -e "${CYAN}  MASSIVE TEST RUN${NC}"
echo -e "${CYAN}  $(date -u +%Y-%m-%d\ %H:%M:%S\ UTC)${NC}"
echo -e "${CYAN}========================================${NC}"

for test_entry in "${TESTS[@]}"; do
    IFS=':' read -r scenario workers release_delay rounds <<< "$test_entry"
    scenario_file="$SCENARIOS_DIR/${scenario}.json"
    
    if [ ! -f "$scenario_file" ]; then
        echo -e "${RED}[SKIP] Scenario file not found: $scenario_file${NC}"
        continue
    fi
    
    success_count=0
    
    echo -e "\n${BLUE}=== ${scenario} (workers=$workers, rounds=$rounds) ===${NC}"
    
    for ((i=1; i<=rounds; i++)); do
        TOTAL_TESTS=$((TOTAL_TESTS + 1))
        printf "  Round %d/%d... " $i $rounds
        
        if run_single_test "$scenario_file" "$workers" "$release_delay"; then
            echo -e "${GREEN}SUCCESS${NC}"
            success_count=$((success_count + 1))
            TOTAL_SUCCESS=$((TOTAL_SUCCESS + 1))
        else
            echo -e "${RED}FAILED${NC}"
        fi
        
        # Extract stats
        stats=$(get_stats "$PREWARM_LOG")
        echo "$scenario,$workers,$i,$success,$stats" >> "$RESULTS_FILE"
    done
    
    rate=$((success_count * 100 / rounds))
    echo -e "  ${CYAN}Result: $success_count/$rounds ($rate%)${NC}"
done

# Summary
echo -e "\n${CYAN}========================================${NC}"
echo -e "${CYAN}  FINAL SUMMARY${NC}"
echo -e "${CYAN}========================================${NC}"
echo -e "Total tests: $TOTAL_TESTS"
echo -e "Total successes: $TOTAL_SUCCESS"
if [ $TOTAL_TESTS -gt 0 ]; then
    overall_rate=$((TOTAL_SUCCESS * 100 / TOTAL_TESTS))
    echo -e "Overall success rate: ${GREEN}$overall_rate%${NC}"
fi
echo -e "\nResults saved to: $RESULTS_FILE"
