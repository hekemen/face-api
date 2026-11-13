#!/bin/sh
# reenroll-users.sh - re-enroll every user of the face API from the camera.
#
# Each user stands in front of the camera in turn. The script captures
# full-resolution frames and replaces that user's enrolled photos.
#
# Usage:
#   scripts/reenroll-users.sh [user ...]
#
# With no arguments every enrolled user is re-enrolled, in the order the
# API reports them.
#
# Environment:
#   FACE_API_URL   API base URL   (default http://pi.home.arpa/face)
#   FACE_API_TOKEN API token      (required)
#   CAMERA_URL     RTSP URL       (default: read from the Pi's secret)
#   PICTURES       photos per user (default 5)
set -e

API="${FACE_API_URL:-http://pi.home.arpa/face}"
TOKEN="${FACE_API_TOKEN:?set FACE_API_TOKEN to the API token}"
PICTURES="${PICTURES:-5}"

if [ -z "$CAMERA_URL" ]; then
  CAMERA_URL=$(kubectl --kubeconfig "$HOME/.kube/config.pi" get secret face-api-secrets \
    -n face-api -o jsonpath='{.data.rtsp-url}' 2>/dev/null | base64 -d || true)
fi
if [ -z "$CAMERA_URL" ]; then
  echo "set CAMERA_URL, or make kubectl --kubeconfig ~/.kube/config.pi work" >&2
  exit 2
fi

# The users to re-enroll: the ones named on the command line, or all of them.
if [ $# -gt 0 ]; then
  USERS="$*"
else
  USERS=$(curl -s --max-time 30 -H "X-Token: $TOKEN" "$API/users" |
    python3 -c "import sys,json; print(' '.join(u['name'] for u in json.load(sys.stdin)['users']))")
fi

echo "Re-enrolling: $USERS"
echo "The camera must see a face for each capture."
echo

for name in $USERS; do
  echo "=== $name ==="
  go run ./cmd/reenroll -url "$CAMERA_URL" -api "$API" -token "$TOKEN" -name "$name" -pictures "$PICTURES"
  echo
done

echo "=== current state ==="
curl -s --max-time 30 -H "X-Token: $TOKEN" "$API/users" | python3 -c "
import sys, json
for u in json.load(sys.stdin)['users']:
    print('  %-10s pictures=%d  alignment=%r' % (u['name'], len(u.get('pictures') or []), u.get('alignment')))
"
