#!/usr/bin/env sh
set -eu

curl -X POST "${BASE_URL:-http://localhost:8080}/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "targetUrl": "https://httpbin.org/post",
    "method": "POST",
    "headers": {"Content-Type": "application/json"},
    "body": {"event": "user_registered", "userId": "u_123"},
    "idempotencyKey": "demo-001"
  }'
