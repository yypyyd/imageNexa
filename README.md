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
- 模型目录是闭集。下游不能自行创建模型 ID。
- 同一产品若有多个渠道，各自使用带渠道前缀的公开 ID；ChatGPT 的 GPT Image 2 使用未加前缀的 `gpt-image-2`。

ChatGPT 账号使用导入的 access token。管理端的“刷新额度”只更新可用次数，不会续期 token；JWT 到期后账号会退出调度，需重新导入有效 token。

## 鉴权

所有 `/v1` 请求都使用标准 OpenAI Bearer 头：

```http
Authorization: Bearer sk-your-api-key
```

不支持 `x-api-key`、Query 参数、Cookie 或自定义鉴权头。超级管理员在控制台创建或轮换 API Key；明文 Key 只显示一次，服务端只持久化哈希和预览。每个 Key 可单独设置并发上限。

管理端使用 HttpOnly、SameSite=Strict 会话 Cookie。写请求还必须携带会话绑定的 `X-CSRF-Token`，管理员密码和会话凭据不会保存在浏览器 localStorage。账号列表可将一次文本、图片或视频能力测试固定到指定上游账号；测试接口和测试产物同样只允许管理员会话访问。

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

`GET /v1/models` 只返回以下 21 个公共 ID。同一产品的多个渠道不会合并。Oreate、Runway 和 Custom 已退出公开目录。内部 route ID 和上游模型名不能作为 API 的 `model` 值。

### 文本（4）

- `gpt-5-5-mini`
- `gpt-5-5-thinking`
- `grok-4.5`
- `grok-chat-fast`

### 图片（10）

- `gpt-image-2`
- `byteplus-gpt-image-2`
- `adobe-gpt-image-2`
- `seedream-5.0-pro`
- `seedream-5.0-lite`
- `byteplus-nano-banana-2`
- `adobe-nano-banana-2`
- `byteplus-nano-banana-pro`
- `adobe-nano-banana-pro`
- `grok-imagine-image`

### 视频（7）

- `kling-3`
- `kling-o3`
- `adobe-seedance-2.0`
- `adobe-seedance-2.0-fast`
- `dola-seedance-2.5`（Dola 专用，30 秒）
- `grok-imagine-video`
- `firefly-video`

## 统一路由与账号调度

下游只提交所选渠道的 canonical 模型 ID。2API 会：

1. 按操作类型、比例、分辨率、时长和参考素材能力筛选可用 route。
2. 在 route 绑定的账号中排除停用、冷却、鉴权失效或已知额度不足的账号；并发已满的合格账号保留在候选队尾，以便槽位释放后继续使用。
3. 按 route 优先级、账号权重、并发余量和该模型对应额度桶选择账号；同等条件下优先匹配最合适的可用余额，避免低成本模型耗尽大余额账号。
4. 在提交上游前原子预占账号并发和额度；确定未受理时释放预占。
5. 每次生成结束都重新读取或对账所选账号的上游额度，并更新管理端显示与下一次调度依据。

鉴权失效、额度不足和安全可重试的临时错误可以切换账号或 route。临时失败会排除当前账号并立刻尝试未用过的号：连续 3 个号返回同一类临时错误即视为池级故障并停止；不同错误最多换 6 个不同账号。一旦上游已经受理任务，或提交结果无法确定，系统不会换号重提，避免重复生成和重复扣费。唯一的账号级例外是 BytePlus GPT Image 2 返回已知的 Beta 执行终态失败，且该账号提交前一次、失败后三次实时余额均明确一致；此时可在同一 BytePlus route 内依次尝试最多 6 个不同账号。每个失败账号都必须独立通过四次余额未变化证明；系统不回已尝试账号、不进入普通临时换号预算、也不切换其他 route。任一余额未知、发生变化、探测失败或提交结果不明确，换号链立即停止。图片和视频创建请求应发送稳定且唯一的 `Idempotency-Key`。图片请求未发送时，服务端会为本次请求生成一个，并通过 `Idempotency-Key` 与 `Location` 响应头返回，以便恢复已受理任务；自动生成的 Key 不会自动出现在客户端下一次重试中，不能替代客户端主动复用。

## BytePlus 账号导入

BytePlus 使用 Lumina 官网的完整 Cookie 作为请求凭据。可以粘贴 Cookie Header，或粘贴包含 `cookie_string` / `cookies[]` 的浏览器导出 JSON；后端只信任 Cookie 中的身份并重新读取资料和额度。若导入对象还包含 `email` 与 `password`，两者只写入私有续期记录，不会出现在管理接口或日志中。

Cookie 必须同时包含有效会话信息和非空 `csrfToken`。单独的 CSRF 值、Bearer Token、账号密码或不完整 Cookie 都不能导入。服务端从 Cookie 派生 `X-Csrf-Token`，导入后立即向上游校验身份和真实额度，并按 BytePlus 支持的 canonical route 建立候选绑定；后续调用若得到明确的模型无权限证据，只禁用该账号对应的 route 绑定，不影响同账号的其他模型。

Lumina 当前登录会话约 48 小时。系统从 `digest` JWT 的 `exp` 记录本地调度截止时间：已明确过期的账号不会承接普通流量，管理端会显示临期或过期状态。`AccountID` 只经 SHA-256 后用于识别同一账号；重新登录后导入新 Cookie 会覆盖原账号行，不会每次新建重复账号。

配置了账密的 Lumina 账号由 2API 自己续期，不依赖外部注册服务：后台按 `digest.exp` 在到期前 6 小时进入续期队列，使用 BytePlus 的密码登录协议换取新 Cookie，校验新旧 `AccountID` 一致后原位覆盖。失败按 5–60 分钟退避重试；旧 Cookie 在真实到期前仍可继续调度。没有账密的旧导入保持手工重导模式。

Cookie 是高敏感凭据：不要写入日志、截图、文档、`.env` 或 Git。

## Adobe 账号导入

Adobe 可以继续导入纯 Cookie；系统会按 Adobe SherlockSdk 的当前协议在本地自动创建基础 `x-arp-session-id`（随机 v4 会话 UUID 的紧凑 JSON，再做标准 Base64），不需要额外代理请求。若浏览器导出 JSON 已包含更完整的 Adobe ARP 值（也兼容常见字段别名），则优先保存并沿用该值。ARP 只在图片/视频生成提交时携带，后续只重导纯 Cookie 不会清除或轮换已有会话。

Cookie 和 ARP 都不会出现在账号列表、日志或 API 响应中。升级时会自动为缺失 ARP 的旧 Adobe 账号补齐基础会话，无需重新导入；浏览器侧 Forter/BFP 指纹属于异步增强信息，服务端不会为了获取它们加载整套页面资源。

## Dola 账号导入

Dola（豆包国际版，dola.com）只通过公共模型 `dola-seedance-2.5` 提供视频（上游即 Dreamina Seedance 2.5）。导入物是 dola.com 的完整浏览器 Cookie；Cookie 必须同时包含有效 `sessionid` 与 `s_v_web_id`（通常还带 `msToken`），粘贴到"账号"页或导入文本框即可自动识别为 Dola 凭据。同一 `sessionid` 重复导入会原位覆盖，不会新建重复账号。

要求与限制：

- **出口 IP**：dola.com 仅在日韩等地区可访问。系统设置里为每个渠道填写提取 API，并发时批量领取出口并按账号粘性分配；Dola 未单独填写时回退到默认提取 API 或回退出站网关。没有可用出口时国内服务器无法使用。
- **能力范围**：仅视频（30s，比例 1:1、3:4、4:3、9:16、16:9、21:9，720p）。支持文生视频，以及最多 10 张参考图的图生视频（`input_reference`）。不支持参考视频/音频。
- **额度**：每号每天 2 次，每次仅生成 30 秒视频，调度单位为 `generations`。提交前原子预占 1 次，第三次不再调度该账号。提交前明确失败可退次；已受理、提交结果不明、成品下载失败不退次且不自动重发。按 UTC 日期（北京时间 08:00）隔离记账；上游提示每日额度耗尽时封存当天剩余额度，上游重置时间仍以实际返回为准。
- **有效期**：Cookie 约 60 天有效；导入时后台会校验会话，失效 Cookie 会被标记停用。

Cookie 是高敏感凭据：不要写入日志、截图、文档、`.env` 或 Git。

## 快速部署

要求：Docker Engine 与 Docker Compose v2。Compose 会启动 PostgreSQL、Redis、RustFS、Go API 和管理员 SPA；HTTP `2000` 端口仅绑定宿主机回环地址，由同机反向代理对外提供 HTTPS。

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
| `REDIS_PASSWORD` | Redis 与后端共享的高熵密码；Compose 会强制 Redis 鉴权 |
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
  -F "model=byteplus-nano-banana-pro" \
  -F "prompt=把背景改成摄影棚" \
  -F "image=@reference.png"

# 创建视频任务
curl https://api.example.com/v1/videos \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: video-order-001" \
  -d '{"model":"kling-3","prompt":"镜头缓慢穿过霓虹街道","seconds":8,"size":"1280x720"}'
```

图片可通过 `Prefer: respond-async` 获得 `202`，再使用响应中的 `request_id` 查询 `/v1/images/tasks`。即使未主动请求异步模式，只要上游已受理而结果仍在处理中，服务也会立即返回 `202`，不会让反向代理超时后丢失恢复句柄。图片编辑仅接受 PNG、JPEG、GIF 或 WebP 参考图；`mask`、`background` 与 `output_format` 当前会被明确拒绝，不会被静默忽略。视频始终按任务方式创建，轮询 `/v1/videos/:id` 到 `completed` 后再请求 `/content`。

图片和视频的任务、状态及 `/content` 都按创建它们的 API Key 隔离；下载内容时也要携带同一个 Bearer Key。

## 本地开发与检查

后端：

```bash
cd backend
go vet ./...
go build ./cmd/api
```

前端：

```bash
cd frontend
npm ci
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

`GET /v1/models` 和 `GET /v1/models?extended=true` 均返回 `dola-seedance-2.5`。下游拉取后选择该模型即可固定使用 Dola 账号池，`POST /v1/videos` 参数为 `seconds: "30"`、`resolution: "720p"`，可附带最多 10 张 `input_reference`。每个 Dola 账号每天 2 次额度，默认单号并发 1。

### Dola Cookie 导入与协议调度

Dola 导入通过 HTTP 验证 Passport 登录态，再由 Alice 协议分配设备标识。生成请求使用本地 Node + jsdom 执行固定版本签名 SDK，通过 HTTP 提交新会话并查询成片，不启动浏览器，也不自动回退网页生成。验证只证明协议认证与签名可用，不承诺上游有额度或一定生成成功。

每号默认并发 1、每天 2 次、只接受 30s/720p。已提交或结果不明禁止自动重发。视频返回时检查上游实际时长，非 30 秒不会作为成功交付。账号页显示协议会话验证状态，临时失败最多 3 次、间隔 5/10 分钟重试。更新 Cookie、手动启用和重验均不重置每日次数。

运行依赖 `node` 及 `scripts/dola-protocol` 内固定版本 SDK 和 npm lock；可用 `DOLA_PROTOCOL_DIR` 指定私有运行目录。签名在本地完成，不上传 Cookie 到签名服务。

协议提交与轮询固定使用同一个账号代理会话，避免等待期间切换出口。视频并发锁覆盖 35 分钟任务超时并留 5 分钟余量，防止长任务未结束就放行同号第二个请求。上游明确拒绝时长且确认未启动视频任务时，及时结束并释放预占；已经启动后的内容拒绝保留扣次，不自动重发。
