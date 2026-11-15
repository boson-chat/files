# files

Boson file uploads with a rolling per-user quota. Go service, image
`harbor.floret.dev/boson/files`. It serves the network's files host
(`files.<domain>`), which must be its own origin: never the app or auth host.

- Accounts sign in with auth-service access tokens (`Authorization: Bearer`,
  checked against the issuer's JWKS). Guests get a short-lived guest token.
- Every user has a quota per **rolling window**: `used = sum(size)` of the
  user's uploads created in the last `window`, counting deleted and expired
  files. **Deleting doesn't refund.** The file itself is removed at once.
- Files expire `retention` after upload (default 3 days), when the expiry
  worker removes them from storage. The ledger row stays until it has also
  left the window.
- Storage: `local` volume (the default) or any S3-compatible bucket (MinIO,
  Cloudflare R2, AWS). Uploads stream straight into storage, and nothing
  holds a whole file in memory.

Defaults: files expire after 72h, 5 GB per account per 72h, 100 MB per
file. Guests get 500 MB per 72h and 25 MB per file. One IP's guest tokens
share 3 × the guest quota.

## API (public listener, `:8080`)

| | |
|---|---|
| `GET /v1/config` | `{enabled, guest_enabled, accounts_enabled, limits: {account: {quota_bytes, window_seconds, max_file_bytes}, guest: {…}}, retention_seconds}` |
| `POST /v1/guest-token` | `201 {token, expires_at, limits}`. Rate-limited per client IP (`429 rate_limited`). `403 guest_disabled` when guests are off. Send the token back as `Authorization: Guest <token>`. |
| `GET /v1/me/usage` | `{used, quota, window_seconds, max_file, frees: [{bytes, at}]}`. `frees` is when space comes back, oldest first. |
| `POST /v1/files` | Raw body. Headers: `Content-Type`, `X-Filename` (URL-encoded) and `Content-Length` (required, else `411`). Returns `201 {id, url, name, size, content_type, created_at, expires_at, width?, height?}`. |
| `GET /v1/me/files` | `{files: [...]}`: your live files, newest first. |
| `DELETE /v1/files/{id}` | `204`. Only your own files (`404` otherwise). No refund. |
| `GET /v1/files/{id}` | Public metadata of a live file, the same shape as the upload response. Useful for file cards. |
| `GET /f/{id}/{name}` | Serves the file (`GET` and `HEAD`, with `Range`). The name is cosmetic. `404` for unknown ids, and `410` with a small "this file has expired" page for expired or deleted ones. |

### Upload errors

All errors are `{error, message, code, …}`.

- `413 quota_exceeded {used, quota, window_seconds, frees_at}`. `frees_at`
  is when enough of the oldest uploads leave the window for this file to
  fit. It is `null` when the file can never fit. For a guest over the
  per-IP total, `used` and `quota` are the IP's.
- `413 file_too_large {max}`
- `507 storage_full`: the server is out of space. Tell the user "server
  storage is full, try again later".
- `503 uploads_disabled`
- `403 guest_disabled`
- `401 unauthenticated`, `401 invalid_guest_token`
- `400 empty_file`
- `400 incomplete_body`: the body was shorter than `Content-Length`.

### Serving headers

Every file response has these headers:

- `X-Content-Type-Options: nosniff`
- `Content-Security-Policy: sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'`
- `Cache-Control: public, max-age=<seconds left>`
- `Cross-Origin-Resource-Policy: cross-origin`
- `Access-Control-Allow-Origin: *`
- `ETag`
- `X-File-Expires-At`
- `X-File-Content-Type` (the stored type)

Only these types are served `inline` with their own `Content-Type`: png,
jpeg, gif, webp, avif, mp4, webm, mp3, ogg, wav, pdf and `text/plain`
(served as `text/plain; charset=utf-8`). Everything else, svg and html
included, is `Content-Disposition: attachment` with type
`application/octet-stream`.

If a body sniffs as HTML or XML, it is never stored under an inline media
type.

Width and height are recorded for png, jpeg, gif and webp. Metadata is not
stripped.

With the s3 driver and `public_base_url` set, inline types are redirected
(302) to the bucket or CDN. Everything else is still proxied, so the
headers above always apply. The default is to proxy everything.

## Internal API (`:3001`, `X-Internal-Token: $INTERNAL_TOKEN`)

| | |
|---|---|
| `GET /internal/files/status` | `{ok, storage: {driver, ok, error, latency_ms, free_bytes, total_bytes, used_bytes, min_free_bytes, capacity_bytes, full}, totals: {live_files, live_bytes, stored_bytes, window_uploads, window_bytes, window_users, pending, awaiting_purge}, config, config_file, restart_required, errors: [{at, op, error}]}` |
| `GET /internal/files?user=&q=&status=&cursor=&limit=` | `{files: [...], next_cursor}`. Each file is the public view plus `user, user_kind, status, deleted_at, deleted_by`. `q` matches the name (substring) or the id. `status` is `live` (the default), `all`, `ready`, `pending`, `deleted` or `expired`. |
| `DELETE /internal/files/{id}` | `204`. Moderation delete, logged with `X-Acting-User`. |
| `GET /internal/files/usage?user=` | The usage, plus `user, user_kind, live_files, live_bytes, override, defaults`. |
| `PUT /internal/quota/{user}` | Body `{quota_bytes, max_file_bytes}` (either may be `null`, meaning the default), or `null` to remove the override. Returns the usage. `DELETE` also removes it. |
| `GET /internal/config/status` | Status of the stack config file (no token needed). |
| `GET /metrics`, `/healthz`, `/readyz` | |

`user` is an auth user id (`usr_…`) or a guest key (`gst_…`, shown in the
file list).

## Config

### `files:` section of `config/boson.yaml` (`BOSON_CONFIG`)

The section is hot-reloaded: a key in the file wins over the env value, and
removing a key resets it. An invalid file keeps the last good config, and
`/internal/config/status` gives the line and column of the problem.

The storage driver settings (`driver`, `s3.*`) apply after a restart, and
the status lists them under `restart_required`. Everything else applies
live.

```yaml
files:
  enabled: true
  retention: 72h
  window: 72h
  account: { quota: 5GB, max_file: 100MB }
  guest:   { enabled: true, quota: 500MB, max_file: 25MB }
  storage:
    driver: local            # local | s3
    min_free: 2GB            # refuse uploads (507 storage_full) that would leave less free
    capacity: 15GB           # s3 only, optional: total budget (S3 has no free-space API)
    s3: { endpoint: "", bucket: "", region: auto, path_style: true, public_base_url: "" }
```

Sizes are a byte count or a number with a unit: `KB`/`MB`/`GB`/`TB` are
powers of 1000, and `KiB`/`MiB`/`GiB`/`TiB` are powers of 1024. Durations
use Go syntax (`72h`).

**Storage guard:**

- **local:** the guard checks the volume's free space (statfs).
- **s3:** set `capacity` to a total budget. The guard checks it against the
  bytes this service has in the bucket. Without `capacity`, s3 has no
  guard.

### Env

| Var | Default | |
|---|---|---|
| `FILES_PUBLIC_URL` | `http://localhost:8080` | Base of file URLs, e.g. `https://files.boson.chat` |
| `DATABASE_URL` | | Postgres (`files` database). Migrations run on start (`AUTO_MIGRATE`). |
| `AUTH_ISSUER` / `AUTH_JWKS_URL` / `AUTH_AUDIENCE` | — / `$AUTH_ISSUER/.well-known/jwks.json` / `boson` | Empty issuer: account uploads are off. |
| `FILES_GUEST_SECRET` | | HMAC key for guest tokens, 16+ chars. Empty: guests are off. |
| `INTERNAL_TOKEN` | | Admin API token. Empty: `/internal/files*` returns `503`. |
| `BOSON_CONFIG` | | Path of the stack config file. |
| `FILES_S3_ACCESS_KEY` / `FILES_S3_SECRET_KEY` | | S3 keys (never in the file). |
| `HTTP_ADDR` / `INTERNAL_ADDR` | `:8080` / `:3001` | |
| `TRUSTED_PROXIES` | | CIDRs whose `X-Forwarded-For` is trusted. Needed for per-IP guest limits behind Traefik. |
| `FILES_DATA_DIR` | `/data` (image) | local driver |
| `FILES_GUEST_TOKEN_TTL` / `FILES_GUEST_TOKENS_PER_HOUR` / `FILES_GUEST_IP_FACTOR` | `24h` / `10` / `3` | |
| `FILES_UPLOAD_TIMEOUT` | `30m` | Read timeout of one request. |
| `FILES_EXPIRY_EVERY` / `FILES_PENDING_TIMEOUT` | `1m` / `1h` | Expiry worker interval and stale-reservation age. |
| `FILES_ENABLED`, `FILES_RETENTION`, `FILES_WINDOW`, `FILES_ACCOUNT_QUOTA`, `FILES_ACCOUNT_MAX_FILE`, `FILES_GUEST_ENABLED`, `FILES_GUEST_QUOTA`, `FILES_GUEST_MAX_FILE`, `FILES_STORAGE_DRIVER`, `FILES_STORAGE_MIN_FREE`, `FILES_STORAGE_CAPACITY`, `FILES_S3_ENDPOINT`, `FILES_S3_BUCKET`, `FILES_S3_REGION`, `FILES_S3_PATH_STYLE`, `FILES_S3_PUBLIC_BASE_URL` | contract defaults | Env fallbacks for the `files:` keys. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` (+ `_METRICS_` / `_TRACES_`) | | OTLP push of metrics and traces. |

## How uploads work

1. Check the limits: `max_file` (with any per-user override), then the
   storage guard.
2. **Reserve.** In one transaction, take advisory locks on the quota keys
   (the user, plus the IP key for guests), sum the window and insert a
   `pending` row. Concurrent uploads therefore can't go over the quota.
3. Stream the body into storage, exactly `Content-Length` bytes. Images are
   measured on the way through. The s3 driver uses multipart upload with
   8 MiB parts, so memory use stays bounded.
4. On success the row becomes `ready`. On failure (client abort, short
   body, storage error) the reservation is deleted and the space released.

Each expiry worker run (leader-locked, so one replica at a time) does four
things:

- marks files past `expires_at` as expired;
- purges deleted and expired bodies from storage, retrying failures;
- releases reservations older than `FILES_PENDING_TIMEOUT`;
- prunes purged rows that have left the window.

## Development

```sh
make dev-up            # Postgres :5442 + MinIO :9000 (docker compose)
cp .env.example .env
make run               # go run ./cmd serve
make test              # unit tests (Postgres/MinIO tests skip)
make test-integration  # everything against the compose Postgres + MinIO
docker compose --profile app up --build   # the service in a container, on MinIO
```

The Postgres integration tests (`test/integration`) cover:

- the rolling window with an injectable clock, including the contract
  example: 5 GB / 3 days, day 1 uploads 1 GB, day 3 hits 5 GB, day 4 has
  1 GB free;
- deleting doesn't refund;
- concurrent uploads never exceed the quota;
- guest per-token and per-IP caps, and the token rate limit;
- serving headers for svg, html and spoofed types;
- Range, HEAD and ETag;
- the expiry worker and pruning;
- releasing stale reservations;
- the storage guard;
- the internal API;
- config file reload.

The s3 driver is tested against MinIO in CI (service container).

## Release

Conventional commits on `main` run semantic-release (tag and changelog).
The release then builds a multi-arch (amd64 and arm64) image and pushes it
to `harbor.floret.dev/boson/files:<version>` and `:latest`. This uses the
org secrets `HARBOR_USERNAME` and `HARBOR_PASSWORD`.
