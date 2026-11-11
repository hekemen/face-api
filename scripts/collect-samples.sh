#!/bin/sh
# collect-samples.sh - capture labeled face-match samples from the camera.
#
# Each run fires one MQTT burst at the camera and records what the system
# scored, together with a label describing who was actually in front of the
# camera. The labels turn the similarity scores into ground truth, which is
# what lets us choose a match threshold from measurements instead of guesses.
#
# Usage:
#   collect-samples.sh <label> [count]
#
#   <label>  who was in front of the camera: a user's name, or "empty" when
#            nobody was there. "empty" is what measures the false-positive
#            floor; a name measures how well that person is recognized.
#   [count]  how many captures to take (default 1).
#
# Environment:
#   FACE_API_URL     API base URL     (default http://pi.home.arpa/face)
#   TRIGGER_BIN      path to face-api-trigger (default: looked up in PATH)
#   FACE_API_TOKEN   API token        (required)
#   MQTT_BROKER      MQTT broker      (default tcp://192.168.178.35:1883)
#   MQTT_TOPIC       MQTT base topic  (default face/scan)
#   SAMPLES_CSV      output file      (default samples.csv)
set -e

LABEL="${1:?usage: collect-samples.sh <label> [count]}"
COUNT="${2:-1}"

API="${FACE_API_URL:-http://pi.home.arpa/face}"
TOKEN="${FACE_API_TOKEN:?set FACE_API_TOKEN to the API token}"
BROKER="${MQTT_BROKER:-tcp://192.168.178.35:1883}"
TOPIC="${MQTT_TOPIC:-face/scan}"
OUT="${SAMPLES_CSV:-samples.csv}"
TRIGGER_BIN="${TRIGGER_BIN:-face-api-trigger}"

if [ ! -f "$OUT" ]; then
  echo "timestamp,label,audit_status,name,similarity,matched,duration_ms,reason" > "$OUT"
fi

i=1
while [ "$i" -le "$COUNT" ]; do
  # Mark the moment before the trigger so the right audit row can be picked.
  MARK=$(date -u +%Y-%m-%dT%H:%M:%SZ)

  # Fire the trigger and wait for the result.
  PAYLOAD=$("$TRIGGER_BIN" "$BROKER" "$TOPIC")
  REASON=$(printf '%s' "$PAYLOAD" | python3 -c "
import json, sys
try:
    print(json.load(sys.stdin).get('reason', '') or '')
except Exception:
    print('')
")

  # Read the outcome back from the audit log: it is the authoritative record
  # and it distinguishes 'no face' from 'below threshold', which the MQTT
  # payload does not.
  ROW=$(curl -s --max-time 30 -H "X-Token: $TOKEN" \
    "$API/api/audit?endpoint=stream-check&per_page=5" |
    MARK="$MARK" LABEL="$LABEL" REASON="$REASON" python3 -c "
import csv, json, os, sys
mark = os.environ['MARK']
d = json.load(sys.stdin)
rows = [e for e in d.get('entries', []) if (e.get('time') or '') >= mark]
if not rows:
    rows = d.get('entries', [])[:1]
if not rows:
    sys.exit(0)
e = rows[0]
csv.writer(sys.stdout).writerow([
    e.get('time') or '',
    os.environ.get('LABEL', ''),
    e.get('status', ''),
    e.get('name', '') or '',
    '%f' % (e.get('similarity') or 0),
    str(e.get('matched', False)),
    str(e.get('duration_ms', 0)),
    os.environ.get('REASON', ''),
])
")

  if [ -z "$ROW" ]; then
    printf '%s,%s,TIMEOUT,,,,,\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$LABEL" >> "$OUT"
    echo "[$i/$COUNT] TIMEOUT - no result (logged as such)"
  else
    printf '%s\n' "$ROW" >> "$OUT"
    printf '%s\n' "$ROW" | python3 -c "
import csv, sys
r = next(csv.reader(sys.stdin))
print('[%s/%s] label=%-12s status=%-11s name=%-10s sim=%.4f matched=%-5s dur=%sms'
      % ('$i', '$COUNT', r[1], r[2], r[3] or '-', float(r[4]), r[5], r[6]))
"
  fi

  i=$((i + 1))
  [ "$i" -le "$COUNT" ] && sleep 3
done

echo ""
echo "samples appended to $OUT"
