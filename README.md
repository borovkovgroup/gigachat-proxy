# gigachat-proxy

Минимальный OpenAI-compatible proxy для GigaChat (Sber). Превращает OAuth2-flow GigaChat в стандартный Bearer-API для клиентов которые ожидают OpenAI shape: LightRAG, LiteLLM, openai-python и т.п.

## Что умеет

- **OAuth2 token refresh** в фоновой горутине (80% TTL, sync first refresh при старте — fail-fast)
- **`httputil.ReverseProxy`** — chat completions, embeddings, models, любые будущие endpoints проксируются прозрачно
- **Streaming (SSE)** — `FlushInterval=-1` + Flusher-aware writer, chunks отдаются клиенту немедленно
- **Concurrent limit** через chan-semaphore (защита от GigaChat rate limit)
- **Tuned `http.Transport`** — `MaxIdleConnsPerHost=64` (vs Go default `2`)
- **`/healthz`** возвращает 200 только когда токен готов — для compose `depends_on: condition: service_healthy`
- **distroless image** ~17 MB

## Quick start

```bash
# Build
docker build -t gigachat-proxy:dev .

# Run (token = base64(client_id:client_secret) из Sber Studio)
docker run -d --name gigachat-proxy \
  -e GIGA_AUTH_TOKEN=<your-base64-token> \
  -e GIGA_SCOPE=GIGACHAT_API_CORP \
  -p 8080:8080 \
  gigachat-proxy:dev

# Smoke test
PROXY_URL=http://localhost:8080 ./smoke/test.sh
```

## Env vars

| Var | Default | Notes |
|---|---|---|
| `GIGA_AUTH_TOKEN` | (required) | base64 client_id:client_secret из Sber Studio |
| `GIGA_SCOPE` | `GIGACHAT_API_PERS` | `_PERS` / `_B2B` / `_CORP` |
| `GIGA_API_URL` | `https://gigachat.devices.sberbank.ru` | без `/api/v1` — proxy сам делает rewrite |
| `GIGA_OAUTH_URL` | `https://ngw.devices.sberbank.ru:9443/api/v2/oauth` | |
| `GIGA_CONCURRENT_LIMIT` | `16` | Семафор. Под `_PERS` снизить до 1-2 |
| `GIGA_TIMEOUT` | `5m` | Per-request timeout |
| `GIGA_SKIP_TLS_VERIFY` | `true` | Sber CA не в стандартном Go truststore |
| `LISTEN_ADDR` | `:8080` | |
| `LOG_LEVEL` | `info` | `debug` / `warn` / `error` |

## Endpoints

- `POST /v1/chat/completions` — chat (с/без `stream`)
- `POST /v1/embeddings`
- `GET /v1/models`
- `GET /healthz` — 200 если токен готов, 503 если нет
