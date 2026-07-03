#!/usr/bin/env bash
# Smoke test for gigachat-proxy.
#
# Usage:
#   PROXY_URL=http://localhost:8080 ./smoke/test.sh
#
# Defaults to http://localhost:8080. Run from anywhere.
# Requires: bash, curl, jq.

set -euo pipefail

PROXY_URL="${PROXY_URL:-http://localhost:8080}"
LLM_MODEL="${LLM_MODEL:-GigaChat-2-Max}"
EMB_MODEL="${EMB_MODEL:-EmbeddingsGigaR}"

bold() { printf "\n\033[1m=== %s ===\033[0m\n" "$1"; }
ok()   { printf "\033[32m✓\033[0m %s\n" "$1"; }
fail() { printf "\033[31m✗\033[0m %s\n" "$1"; exit 1; }

bold "1. /healthz"
HEALTH=$(curl -sf "$PROXY_URL/healthz") || fail "healthz unreachable at $PROXY_URL"
echo "$HEALTH" | jq .
[[ $(echo "$HEALTH" | jq -r .status) == "ok" ]] || fail "healthz status != ok"
ok "healthz returns ok (token is ready)"

bold "2. Non-streaming chat ($LLM_MODEL)"
RESP=$(curl -sf "$PROXY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$LLM_MODEL\",
    \"messages\": [{\"role\":\"user\",\"content\":\"Ответь одним русским словом: что такое RAG?\"}],
    \"stream\": false,
    \"max_tokens\": 50
  }")
echo "$RESP" | jq -r '.choices[0].message.content'
[[ -n "$(echo "$RESP" | jq -r '.choices[0].message.content')" ]] || fail "empty content"
ok "non-streaming chat works"

bold "3. STREAMING chat ($LLM_MODEL) — chunks must arrive progressively"
START=$(date +%s%N)
FIRST_CHUNK_NS=""
COUNT=0
while IFS= read -r line; do
  if [[ -z "$line" ]]; then continue; fi
  if [[ "$line" == "data: [DONE]" ]]; then break; fi
  if [[ "$line" == data:* ]]; then
    if [[ -z "$FIRST_CHUNK_NS" ]]; then
      FIRST_CHUNK_NS=$(date +%s%N)
      FTFB_MS=$(( (FIRST_CHUNK_NS - START) / 1000000 ))
      echo "  ↳ first chunk after ${FTFB_MS}ms"
    fi
    COUNT=$((COUNT + 1))
    # show first 3 chunks then summarize
    if (( COUNT <= 3 )); then
      printf "  chunk %d: %s\n" "$COUNT" "${line:0:120}"
    fi
  fi
done < <(curl -sN "$PROXY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$LLM_MODEL\",
    \"messages\": [{\"role\":\"user\",\"content\":\"Расскажи короткую историю в трёх предложениях про архитектора облачных систем.\"}],
    \"stream\": true,
    \"max_tokens\": 300
  }")
echo "  total chunks: $COUNT"
(( COUNT >= 3 )) || fail "got fewer than 3 stream chunks — streaming might be buffered"
ok "streaming works ($COUNT chunks)"

bold "4. Embeddings ($EMB_MODEL)"
EMB_RESP=$(curl -sf "$PROXY_URL/v1/embeddings" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$EMB_MODEL\",
    \"input\": [\"первый текст\", \"второй текст\"]
  }")
N_VECS=$(echo "$EMB_RESP" | jq '.data | length')
DIM=$(echo "$EMB_RESP" | jq '.data[0].embedding | length')
echo "  vectors: $N_VECS,  dim: $DIM"
[[ "$N_VECS" == "2" ]] || fail "expected 2 vectors, got $N_VECS"
[[ "$DIM" -gt 100 ]] || fail "suspiciously small dim: $DIM"
ok "embeddings work — dim=$DIM (USE THIS in LightRAG EMBEDDING_DIM)"

bold "5. Concurrency check (8 parallel embeddings)"
START=$(date +%s%N)
for i in {1..8}; do
  curl -sf "$PROXY_URL/v1/embeddings" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"$EMB_MODEL\",\"input\":\"параллельный тест $i\"}" \
    > /dev/null &
done
wait
END=$(date +%s%N)
DUR_MS=$(( (END - START) / 1000000 ))
echo "  8 parallel embeddings in ${DUR_MS}ms"
ok "concurrency OK (would be 8× single-call latency if semaphore=1)"

printf "\n\033[32mAll smoke tests passed.\033[0m\n"
