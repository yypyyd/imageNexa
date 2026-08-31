<div align="center">

<img src="frontend/public/favicon.svg" width="80" alt="2API" />

# 2API

**面向文本、图片与视频生成的 OpenAI 兼容 API 网关**

简体中文 | [English](README.en.md) | [设计文档](DESIGN.md)

</div>

## 项目定位

2API 是一个自托管的 API 服务与单一超级管理员控制台。下游只需使用一个 OpenAI 风格的 `Bearer sk-*` API Key 和 canonical 模型 ID；系统在内部完成 Provider 路由、账号选择、并发控制、额度预占、故障转移与额度刷新。

产品边界固定为：

- 对外提供 `/v1` 文本、图片、图片编辑、异步图片任务和视频接口。
- 只保留一个超级管理员，不提供公开注册或公共用户前台。
- 管理端负责模型 route、上游账号、API Key、日志、成品、违禁词和系统设置。
- 模型目录是闭集。下游不使用 Provider 前缀，也不能自行创建模型 ID。
- 同一个 canonical 模型可由多个 Provider/账号承载，Provider 细节不进入公共 API。

## 鉴权

所有 `/v1` 请求都使用标准 OpenAI Bearer 头：

```http
Authorization: Bearer sk-your-api-key
```

不支持 `x-api-key`、Query 参数、Cookie 或自定义鉴权头。超级管理员在控制台创建或轮换 API Key；明文 Key 只显示一次，服务端只持久化哈希和预览。每个 Key 可单独设置并发上限。

管理端使用 HttpOnly、SameSite=Strict 会话 Cookie。写请求还必须携带会话绑定的 `X-CSRF-Token`，管理员密码和会话凭据不会保存在浏览器 localStorage。

首次创建超级管理员还必须输入部署环境中的 `ADMIN_BOOTSTRAP_TOKEN`。初始化请求只通过固定的 `X-Admin-Bootstrap-Token` Header 传递它；管理员一旦创建，数据库单例约束会永久关闭再次初始化。

## 公共 API

除健康检查外，下列接口都需要 `Authorization: Bearer sk-*`。

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/v1/models` | 获取 canonical 模型目录；默认返回严格 OpenAI 五字段模型对象（`shutdown_date` 为 `null`），`?extended=true` 才包含能力扩展 |
| `POST` | `/v1/chat/completions` | 文本对话，支持普通 JSON 与 `stream: true` SSE |
| `POST` | `/v1/images/generations` | 文生图；默认同步，可用 `Prefer: respond-async` 请求异步执行 |
| `POST` | `/v1/images/edits` | `multipart/form-data` 图片编辑，支持一个或多个参考图 |
| `GET` | `/v1/images/tasks?request_id=...` | 查询异步图片任务或按幂等编号恢复结果 |
| `GET` | `/v1/images/:id/content` | 获取图片内容 |
| `POST` | `/v1/videos` | 创建异步视频任务 |
| `GET` | `/v1/videos/:id` | 查询视频任务状态 |
| `GET` | `/v1/videos/:id/content` | 下载视频成品 |

无需 Bearer 的探针：

- `GET /health/live`：进程存活。
- `GET /health/ready`：PostgreSQL、Redis、最新数据库迁移和私有 RustFS Bucket 均已就绪。

错误统一为 OpenAI 风格：

```json
{"error":{"message":"...","type":"invalid_request_error","param":"model","code":"model_not_found"}}
```

## Canonical 模型闭集

`GET /v1/models` 只返回以下 24 个公共 ID。Provider 的上游模型名和 route ID 仅供内部适配，不能作为 API 的 `model` 值。

### 文本（4）

- `gpt-5-5-mini`
- `gpt-5-5-thinking`
- `grok-4.5`
- `grok-chat-fast`

### 图片（6）

- `gpt-image-2`
- `seedream-5.0-pro`
- `seedream-5.0-lite`
- `nano-banana-2`
- `nano-banana-pro`
- `grok-imagine-image`

### 视频（14）

- `veo-3.1`
- `veo-3.1-lite`
- `kling-3`
- `kling-o3`
- `runway-gen-4.5`
- `runway-gen-4-turbo`
- `seedance-2.0`
- `seedance-2.0-fast`
- `seedance-2.0-mini`
- `seedance-1.5-pro`
- `seedance-2.5`
- `grok-imagine-video`
- `luma-ray`
- `firefly-video`

## 统一路由与账号调度

下游只提交 canonical 模型 ID。2API 会：

1. 按操作类型、比例、分辨率、时长和参考素材能力筛选可用 route。
2. 在 route 绑定的账号中排除停用、冷却、鉴权失效、并发已满或已知额度不足的账号。
3. 按 route 优先级、账号权重、并发余量和该模型对应额度桶选择账号；同等条件下优先匹配最合适的可用余额，避免低成本模型耗尽大余额账号。
4. 在提交上游前原子预占账号并发和额度；确定未受理时释放预占。
5. 每次生成结束都重新读取或对账所选账号的上游额度，并更新管理端显示与下一次调度依据。

鉴权失效、额度不足和安全可重试的临时错误可以切换账号或 route。一旦上游已经受理任务，或提交结果无法确定，系统不会换号重提，避免重复生成和重复扣费。图片和视频创建请求应发送稳定且唯一的 `Idempotency-Key`。图片请求未发送时，服务端会为本次请求生成一个，并通过 `Idempotency-Key` 与 `Location` 响应头返回，以便恢复已受理任务；自动生成的 Key 不会自动出现在客户端下一次重试中，不能替代客户端主动复用。

## BytePlus 账号导入

BytePlus 只使用 Lumina 官网的完整 Cookie 作为凭据。可以粘贴 Cookie Header，或粘贴包含 `cookie_string` / `cookies[]` 的浏览器导出 JSON；后端只提取、规范化并保存 Cookie，不信任导出文件中的邮箱、头像、租户或额度字段。

Cookie 必须同时包含有效会话信息和非空 `csrfToken`。单独的 CSRF 值、Bearer Token、账号密码或不完整 Cookie 都不能导入。服务端从 Cookie 派生 `X-Csrf-Token`，导入后立即向上游校验身份和真实额度，并按 BytePlus 支持的 canonical route 建立候选绑定；后续调用若得到明确的模型无权限证据，只禁用该账号对应的 route 绑定，不影响同账号的其他模型。

Cookie 是高敏感凭据：不要写入日志、截图、文档、`.env` 或 Git。

## 快速部署

要求：Docker Engine 与 Docker Compose v2。Compose 会启动 PostgreSQL、Redis、RustFS、Go API 和管理员 SPA；宿主机通过 HTTP `2000` 端口访问。

1. 创建部署环境文件：

```bash
cp .env.example .env
```

PowerShell：

```powershell
Copy-Item .env.example .env
```

2. 修改 `.env`，至少替换所有 `replace-with-*` 值：

| 变量 | 说明 |
|---|---|
| `APP_ENV` | 线上必须为 `production`；仅本机明文 HTTP 调试可显式设为 `development` |
| `APP_TITLE` | 控制台标题 |
| `PUBLIC_BASE_URL` | API 对外规范地址，例如 `https://api.example.com` |
| `CORS_ORIGINS` | 允许访问管理员 API 的控制台 Origin；多个值用逗号分隔 |
| `COOKIE_SECURE` | HTTPS 部署设为 `true`；仅本机 HTTP 调试时设为 `false` |
| `SESSION_COOKIE_NAME` | 管理员会话 Cookie 名 |
| `ADMIN_BOOTSTRAP_TOKEN` | 首次创建唯一超级管理员所需的高熵秘密；至少 32 字节且不能使用模板值 |
| `TRUSTED_PROXY_CIDRS` | 可被后端信任并从右向左解析 `X-Forwarded-For` 的反向代理 CIDR |
| `POSTGRES_DB` / `POSTGRES_USER` / `POSTGRES_PASSWORD` | PostgreSQL 配置；生产密码至少 16 字节，不能使用默认值 |
| `RUSTFS_BUCKET` / `RUSTFS_ACCESS_KEY` / `RUSTFS_SECRET_KEY` | 私有对象存储配置；生产 Access Key 至少 16 字节、Secret Key 至少 32 字节 |

可用以下命令生成初始化令牌：

```bash
openssl rand -hex 32
```

PowerShell：

```powershell
[Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLower()
```

3. 校验并启动：

```bash
docker compose config
docker compose up -d --build
docker compose ps
```

4. 检查状态：

```bash
curl http://localhost:2000/health/live
curl http://localhost:2000/health/ready
```

首次访问控制台时，输入 `.env` 中的初始化令牌并创建唯一超级管理员；令牌不会写入浏览器存储或数据库。然后在“API Key”页面创建下游 Key，在“账号”页面导入上游凭据。

生产环境应在 `2000` 端口前放置自己的 HTTPS 反向代理，并保留 `Host`、以追加方式传递 `X-Forwarded-For`。公开链接只以 `PUBLIC_BASE_URL=https://...` 为准，不信任客户端提供的转发 scheme。生产模式还会强制 HTTPS `CORS_ORIGINS`、`COOKIE_SECURE=true`、非默认数据库密码和非模板 RustFS 密钥；任一缺失时后端拒绝启动。如需穿过额外代理解析真实客户端 IP，还必须只把该代理的精确网段加入 `TRUSTED_PROXY_CIDRS`。项目不负责签发 TLS 证书。

常用运维命令：

```bash
docker compose logs -f backend web
docker compose pull
docker compose up -d --build
```

## 调用示例

先在管理员控制台创建 API Key。

```bash
# 模型目录
curl https://api.example.com/v1/models \
  -H "Authorization: Bearer sk-your-api-key"

# 文本
curl https://api.example.com/v1/chat/completions \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-5-mini","messages":[{"role":"user","content":"你好"}]}'

# 图片
curl https://api.example.com/v1/images/generations \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: image-order-001" \
  -d '{"model":"gpt-image-2","prompt":"极简产品摄影","size":"1024x1024"}'

# 图片编辑
curl https://api.example.com/v1/images/edits \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Idempotency-Key: edit-order-001" \
  -F "model=nano-banana-pro" \
  -F "prompt=把背景改成摄影棚" \
  -F "image=@reference.png"

# 创建视频任务
curl https://api.example.com/v1/videos \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: video-order-001" \
  -d '{"model":"veo-3.1","prompt":"镜头缓慢穿过霓虹街道","seconds":8,"size":"1280x720"}'
```

图片可通过 `Prefer: respond-async` 获得 `202`，再使用响应中的 `request_id` 查询 `/v1/images/tasks`。即使未主动请求异步模式，只要上游已受理而结果仍在处理中，服务也会立即返回 `202`，不会让反向代理超时后丢失恢复句柄。图片编辑仅接受 PNG、JPEG、GIF 或 WebP 参考图；`mask`、`background` 与 `output_format` 当前会被明确拒绝，不会被静默忽略。视频始终按任务方式创建，轮询 `/v1/videos/:id` 到 `completed` 后再请求 `/content`。

图片和视频的任务、状态及 `/content` 都按创建它们的 API Key 隔离；下载内容时也要携带同一个 Bearer Key。

## 本地开发与检查

后端：

```bash
cd backend
go test ./...
go build ./cmd/api
```

前端：

```bash
cd frontend
npm ci
npm test
npm run lint:unused
npm run build
```

## 仓库结构

```text
backend/                 Go API、Provider 适配器、路由与调度
frontend/                Vue 3 单一管理员控制台
docker-compose.yml       完整本地/生产容器编排
.env.example             部署变量模板
DESIGN.md                架构、数据模型、安全边界与调度语义
```

## License

本项目基于 [MIT License](LICENSE) 开源。
