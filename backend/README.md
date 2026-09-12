# 2API Backend

Go data plane and singleton-administrator control plane for 2API. The public API exposes only the canonical text, image, image-edit, and video endpoints documented in the repository [README](../README.md). Provider names, upstream model IDs, account credentials, and account selection stay internal.

## Security contract

- Every `/v1` request requires `Authorization: Bearer sk-*`; alternate headers, cookies, and query credentials are rejected.
- Image and video task/content reads are scoped to the API credential that created the event.
- The control plane has exactly one administrator and uses an HttpOnly, SameSite=Strict session cookie plus CSRF protection.
- First initialization also requires `X-Admin-Bootstrap-Token`, matching the deployment-only `ADMIN_BOOTSTRAP_TOKEN`.
- PostgreSQL stores downstream API-key hashes and durable routing/quota state. Redis stores short-lived concurrency/session state. RustFS stores private generated artifacts.
- The production backend container runs as an unprivileged user. Docker's default seccomp profile prevents Chromium user namespaces, so the Oreate signer uses `--no-sandbox`; its empty environment, ephemeral profile, parent-death/process-group teardown, and restricted provider-only use are mandatory compensating controls.

## Local dependencies

The supported full-stack deployment is `docker compose` from the repository root. To run only the backend during development, start PostgreSQL, Redis, and RustFS, then copy and edit the local template:

```powershell
Copy-Item .env.example .env
go run ./cmd/api
```

Important variables are:

- `POSTGRES_DSN`, `REDIS_ADDR`
- `RUSTFS_ENDPOINT`, `RUSTFS_BUCKET`, `RUSTFS_ACCESS_KEY`, `RUSTFS_SECRET_KEY`
- `ADMIN_BOOTSTRAP_TOKEN`
- `PUBLIC_BASE_URL`, `CORS_ORIGINS`, `COOKIE_SECURE`
- `TRUSTED_PROXY_CIDRS` for reverse proxies that append `X-Forwarded-For`

Never commit the real `.env` or any provider credential. The backend imports BytePlus only from a complete Lumina Cookie header (or a browser export containing `cookie_string`, `cookie_header`, or `cookies[]`).

## Database lifecycle

Migration definitions are compiled from `internal/migrations/definitions.go`; no separate SQL files or manual SQL execution are required. Existing migration names and checksums remain unchanged. Append new versions instead of editing applied definitions.

Forward-only, checksummed migrations create administrator/API-credential identity, the 26-model canonical catalog, model routes, account-route entitlements, quota buckets/reservations, dispatch attempts, and API-key-attributed events. Provider-account deletion cascades through its route bindings, quota buckets, and bucket-owned reservations, while event logs and nullable dispatch history remain. Startup refuses unknown or modified applied migrations. `AutoMigrate` is limited to compatible columns on retained operational tables and does not seed retired models.

## Verification

```powershell
go vet ./...
go build ./cmd/api
```

Operational probes are `GET /health/live` and `GET /health/ready`. Readiness requires PostgreSQL, Redis, the latest migration, and access to the configured private RustFS bucket.

Dola 视频只接受 `30s`，按每号每天 2 次计量。调度使用 `dola.video.daily:YYYY-MM-DD`（UTC）持久化预占，已提交及结果不明的任务不退款、不自动换号重发。000018 迁移保留旧账本，并回填当天可识别用量。迁移定义已合并进 Go 代码，服务器启动时自动执行未应用的版本。

每日 2 次上限按 **Dola 账号**独立计算，不是 API 用户或系统总并发限制。6 个 Dola 账号共可预占 12 次。并发槽位沿用原配置；目前浏览器提交有共享锁，生成结果轮询可重叠执行。

公共模型发现：`dola-seedance-2.5` 由 000019 迁移注册。此入口仅匹配 Dola 路由（30s/720p，文生视频或最多 10 张参考图）。迁移保留原有绑定权限、冷却期和用量。

### Dola Cookie 导入与协议调度

Dola 导入通过 HTTP 验证 Passport 登录态，再由 Alice 协议分配设备标识。生成请求使用本地 Node + jsdom 执行固定版本签名 SDK，通过 HTTP 提交新会话并查询成片，不启动浏览器，也不自动回退网页生成。验证只证明协议认证与签名可用，不承诺上游有额度或一定生成成功。

每号默认并发 1、每天 2 次、只接受 30s/720p，可带最多 10 张参考图。已提交或结果不明禁止自动重发。视频返回时检查上游实际时长，非 30 秒不会作为成功交付。账号页显示协议会话验证状态，临时失败最多 3 次、间隔 5/10 分钟重试。更新 Cookie、手动启用和重验均不重置每日次数。

运行依赖 `node` 及 `scripts/dola-protocol` 内固定版本 SDK 和 npm lock；可用 `DOLA_PROTOCOL_DIR` 指定私有运行目录。签名在本地完成，不上传 Cookie 到签名服务。

协议提交与轮询固定使用同一个账号代理会话，避免等待期间切换出口。视频并发锁覆盖 35 分钟任务超时并留 5 分钟余量，防止长任务未结束就放行同号第二个请求。上游明确拒绝时长且确认未启动视频任务时，及时结束并释放预占；已经启动后的内容拒绝保留扣次，不自动重发。
