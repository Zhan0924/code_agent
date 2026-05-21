h#!/bin/sh
# Test Chat API with real LLM integration
SESSION_ID="6d3ffff3-8146-465b-924c-38d936e05fb4"

echo "=== T5: Chat API (real LLM call) ==="
wget -q -O - --header="Content-Type: application/json" \
  --post-data="{\"session_id\":\"$SESSION_ID\",\"message\":\"What is a goroutine in Go? Answer in 2 sentences.\"}" \
  http://localhost:8080/api/v1/chat

echo ""
echo "=== T6: HITL Trigger (sensitive command) ==="
wget -q -O - --header="Content-Type: application/json" \
  --post-data="{\"session_id\":\"$SESSION_ID\",\"message\":\"Please run: DROP DATABASE production\"}" \
  http://localhost:8080/api/v1/chat

echo ""
echo "=== T7: Multi-turn (uses session history) ==="
wget -q -O - --header="Content-Type: application/json" \
  --post-data="{\"session_id\":\"$SESSION_ID\",\"message\":\"Now explain channels in 1 sentence.\"}" \
  http://localhost:8080/api/v1/chat
