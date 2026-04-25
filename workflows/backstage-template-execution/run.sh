#!/usr/bin/env bash
# Usage:
#   ./run.sh              → trigger with input.json, then poll until done
#   ./run.sh status <id>  → check status of a previous run

set -euo pipefail

DAPR_HTTP_PORT=${DAPR_HTTP_PORT:-3500}
WORKFLOW_NAME="BackstageTemplateWorkflow"

# ── status check ─────────────────────────────────────────────────────────────
if [[ "${1:-}" == "status" ]]; then
  ID=${2:?Usage: ./run.sh status <instanceId>}
  RESP=$(curl -sS "http://localhost:${DAPR_HTTP_PORT}/v1.0-beta1/workflows/dapr/${ID}")
  if ! echo "$RESP" | python3 -c 'import sys,json; json.loads(sys.stdin.read())' 2>/dev/null; then
    echo "Raw response: $RESP"
    exit 1
  fi
  echo "$RESP" | python3 -c "
import sys, json
d = json.load(sys.stdin)
p = d.get('properties', {})
print('Status      :', d.get('runtimeStatus'))
print('CustomStatus:', p.get('dapr.workflow.custom_status',''))
print('Updated     :', d.get('lastUpdatedAt',''))
out = p.get('dapr.workflow.output')
if out:
    print('Output:')
    try:    print(json.dumps(json.loads(out), indent=2))
    except: print(out)
err = p.get('dapr.workflow.failure.error_message')
if err:
    print('Error:', err)
"
  exit 0
fi

# ── trigger ───────────────────────────────────────────────────────────────────
INPUT_FILE=${1:-input.json}
[[ -f "$INPUT_FILE" ]] || { echo "input file not found: $INPUT_FILE"; exit 1; }

# Substitute env vars in the JSON ($DAPR_SERVICE_TOKEN etc.)
INPUT=$(envsubst < "$INPUT_FILE" 2>/dev/null || cat "$INPUT_FILE")

INSTANCE_ID="run-$(date +%s)"
SUMMARY=$(echo "$INPUT" | python3 -c '
import sys, json
d = json.load(sys.stdin)
stages = d.get("stages", [])
print(f"stages   : {len(stages)}")
for i, s in enumerate(stages):
    name = s.get("name") or f"stage{i}"
    tpl  = s.get("templateRef","?")
    dry  = " (dry-run)" if s.get("dryRun") else ""
    print(f"  [{i+1}] {name}: {tpl}{dry}")
')

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "$SUMMARY"
echo " Instance : $INSTANCE_ID"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Dapr workflow start: the request body IS the workflow input (raw JSON).
START_RESP=$(curl -sS -X POST \
  "http://localhost:${DAPR_HTTP_PORT}/v1.0-beta1/workflows/dapr/${WORKFLOW_NAME}/start?instanceID=${INSTANCE_ID}" \
  -H "Content-Type: application/json" \
  -d "$INPUT")
echo "Started: $START_RESP"

echo ""
echo "Watching status (ctrl+c to stop)..."
echo ""

# Poll until terminal state
while true; do
  RESULT=$(curl -sS \
    "http://localhost:${DAPR_HTTP_PORT}/v1.0-beta1/workflows/dapr/${INSTANCE_ID}")

  if ! echo "$RESULT" | python3 -c 'import sys,json; json.loads(sys.stdin.read())' 2>/dev/null; then
    echo "  $(date +%H:%M:%S)  raw: $RESULT"
    sleep 3
    continue
  fi

  STATUS=$(echo "$RESULT" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("runtimeStatus",""))')
  CUSTOM=$(echo "$RESULT" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("properties",{}).get("dapr.workflow.custom_status",""))')

  echo "  $(date +%H:%M:%S)  $STATUS  $CUSTOM"

  case "$STATUS" in
    COMPLETED|FAILED|TERMINATED|Completed|Failed|Terminated)
      echo ""
      ./run.sh status "$INSTANCE_ID"
      break
      ;;
  esac
  sleep 3
done
