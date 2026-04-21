#!/bin/bash
set -e

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR="$ROOT_DIR/bin"
SCENARIOS_DIR="$ROOT_DIR/test/scenarios"

MOCK_PORT=9876

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

cleanup() {
    killall -9 mockserver prewarm 2>/dev/null || true
}
trap cleanup EXIT

build() {
    cd "$ROOT_DIR"
    go build -o "$BIN_DIR/mockserver" ./cmd/mockserver/
    go build -o "$BIN_DIR/prewarm" ./cmd/prewarm/
}

run_test() {
    local scenario="$1"
    local workers="$2"
    local scenario_file="$SCENARIOS_DIR/${scenario}.json"
    
    # JST timing
    JST_NOW=$(TZ='Asia/Tokyo' date '+%H %M')
    JST_HOUR=$(echo $JST_NOW | awk '{print $1}')
    JST_MIN=$(echo $JST_NOW | awk '{print $2}')
    NEXT_MIN=$(( (10#$JST_MIN + 1) % 60 ))
    NEXT_HOUR=$(( 10#$JST_HOUR + (10#$JST_MIN + 1) / 60 ))
    if [ $NEXT_HOUR -ge 24 ]; then NEXT_HOUR=0; fi
    
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

    # Start mock with 180s delay (enough for prewarm)
    "$BIN_DIR/mockserver" -port $MOCK_PORT -scenario "$scenario_file" -release-delay 180 > /tmp/mock.log 2>&1 &
    sleep 2
    
    timeout 240 "$BIN_DIR/prewarm" -config "$ROOT_DIR/config_test.json" > /tmp/prewarm.log 2>&1 || true
    
    killall -9 mockserver prewarm 2>/dev/null || true
    sleep 1
    
    if grep -q "Success=true" /tmp/prewarm.log 2>/dev/null; then
        return 0
    else
        return 1
    fi
}

build

echo -e "${BLUE}=== QUICK MASSIVE TEST ===${NC}"
echo ""

RESULTS=()

# Test matrix: scenario:workers
TESTS=(
    "01_easy:50"
    "01_easy:200"
    "02_hard_race:50"
    "02_hard_race:100"
    "02_hard_race:200"
    "06_extreme_race:50"
    "06_extreme_race:100"
    "06_extreme_race:200"
    "07_ultra_extreme:50"
    "07_ultra_extreme:100"
    "07_ultra_extreme:200"
    "08_cancellation_hope:50"
    "08_cancellation_hope:100"
    "08_cancellation_hope:200"
)

TOTAL=0
SUCCESS=0

for test_entry in "${TESTS[@]}"; do
    IFS=':' read -r scenario workers <<< "$test_entry"
    TOTAL=$((TOTAL + 1))
    
    printf "[$TOTAL/${#TESTS[@]}] %-25s workers=%-3s ... " "$scenario" "$workers"
    
    if run_test "$scenario" "$workers"; then
        echo -e "${GREEN}SUCCESS${NC}"
        SUCCESS=$((SUCCESS + 1))
        RESULTS+=("PASS")
    else
        echo -e "${RED}FAILED${NC}"
        RESULTS+=("FAIL")
    fi
done

echo ""
echo -e "${BLUE}=== SUMMARY ===${NC}"
echo "Total: $SUCCESS/$TOTAL"

if [ $TOTAL -gt 0 ]; then
    RATE=$((SUCCESS * 100 / TOTAL))
    echo "Success rate: ${GREEN}${RATE}%${NC}"
fi

echo ""
echo "Results by scenario:"
for ((i=0; i<${#TESTS[@]}; i++)); do
    IFS=':' read -r scenario workers <<< "${TESTS[$i]}"
    status="${RESULTS[$i]}"
    if [ "$status" = "PASS" ]; then
        echo -e "  ${GREEN}PASS${NC} - $scenario (workers=$workers)"
    else
        echo -e "  ${RED}FAIL${NC} - $scenario (workers=$workers)"
    fi
done
