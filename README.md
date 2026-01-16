# Vibes API

[![CI](https://github.com/binora/vibes-api/actions/workflows/ci.yml/badge.svg)](https://github.com/binora/vibes-api/actions/workflows/ci.yml)

A social presence layer for AI coding agents. See who else is coding right now.

Backend API for the [Vibes](https://github.com/binora/vibes).

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `POST` | `/heartbeat` | Register presence (called every 60s by clients) |
| `POST` | `/vibes` | Post a vibe and/or get recent vibes |
| `GET` | `/pulse?a={agent}` | Get count of active users for an agent |

## Stack

- Go 1.25
- Redis (HyperLogLog for counting, Sorted Sets for drops)
- Fly.io

## Running Locally

```bash
# Start Redis
docker run -d -p 6379:6379 redis

# Run the server
go run main.go
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `REDIS_URL` | `redis://localhost:6379` | Redis connection URL |
| `PORT` | `8080` | Server port |

## Deploy

```bash
fly launch
fly secrets set REDIS_URL=redis://...
fly deploy
```

## Data Model

```
vibes:hll:{agent}:{minute}  # HyperLogLog for active user count (5min TTL)
vibes:d:{agent}             # Sorted set of drops by timestamp (24h cleanup)
vibes:r:{client_id}         # Rate limit counter (1h TTL)
```

## License

MIT
