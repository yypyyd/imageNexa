<div align="center">

<img src="frontend/public/favicon.svg" width="84" alt="Vivid AI" />

<h1>image2api</h1>

**多供应商 AI 文本 / 生图 / 生视频网关 —— 一套 OpenAI 兼容 API,聚合多平台,开箱即用的运营系统**

<sub>线上实例(品牌):[Vivid AI · vividai.run](https://vividai.run)</sub>

**简体中文** | [English](README.en.md)

[![Online Demo](https://img.shields.io/badge/在线体验-vividai.run-7c3aed?style=for-the-badge)](https://vividai.run)

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Vue 3](https://img.shields.io/badge/Vue-3-42b883?logo=vuedotjs&logoColor=white)](https://vuejs.org)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](#-部署)
[![OpenAI Compatible](https://img.shields.io/badge/OpenAI-compatible-412991?logo=openai&logoColor=white)](#-openai-兼容-api)
[![HTTPS](https://img.shields.io/badge/HTTPS-反代自理-lightgrey)](#-部署)
[![Providers](https://img.shields.io/badge/供应商-9%20平台-orange)](#-支持的模型--供应商)
[![Self-hosted](https://img.shields.io/badge/self--hosted-yes-success)](#-部署)
[![License](https://img.shields.io/badge/license-MIT-blue)](#-license)

[在线体验](https://vividai.run) · [功能](#-核心功能) · [部署](#-部署) · [API 文档](#-openai-兼容-api) · [交流群](#-交流--联系)

<br/>

<img src="docs/screenshots/playground.png" alt="image2api — 画图台" width="860" />

</div>

---

## 📖 目录

- [简介](#-简介)
- [界面预览](#-界面预览)
- [核心功能](#-核心功能)
- [支持的模型 / 供应商](#-支持的模型--供应商)
- [OpenAI 兼容 API](#-openai-兼容-api)
- [部署](#-部署)
- [技术栈](#-技术栈)
- [仓库结构](#-仓库结构)
- [Roadmap](#-roadmap)
- [交流 / 联系](#-交流--联系)
- [License](#-license)

## ✨ 简介

**image2api** 把 Adobe Firefly、BytePlus Lumina、OpenAI、Runway、Grok、Leonardo、Krea、Imagine、OreateAI 等平台,以及**任意 OpenAI 兼容上游**的文本 / 图像 / 视频能力,统一封装成**一套 OpenAI 兼容的 API**;背后用多账号池自动调度 —— 权重优先 + 并发感知、额度耗尽自动换号、认证失效自动刷新或判死、临时错误自动重试、token 到期前主动续期 —— 对外提供稳定服务。

它不只是 API 代理:自带**积分计费、CDK 充值、邀请奖励、用户体系、管理后台、现代化画图前端**,一条命令即可跑成一个对外运营的 AI 生成站点 —— 作者的线上实例 **[Vivid AI · vividai.run](https://vividai.run)**(品牌)即基于本项目搭建。

> 💡 前后端**完全开源**(MIT),Go + Vue 3,可自由二开 / 自部署。

**一句话亮点** 🔌 OpenAI 兼容 · 🤖 9 平台十余模型 · 🔁 自动换号 / Token 保活 · 💳 积分 + 在线充值(易支付)+ 代理价 · 🧩 并发分组 · 🎨 画图前端 + 管理后台 · 🐳 一键部署(TLS 反代自理)

## 🖼️ 界面预览

<div align="center">
<sub>🎨 现代化画图前端 · 深 / 浅双主题管理后台 · 数据驱动的运营看板</sub>
</div>

<table>
  <tr>
    <td width="50%"><img src="docs/screenshots/dashboard.png" alt="概览看板" /></td>
    <td width="50%"><img src="docs/screenshots/models.png" alt="模型管理" /></td>
  </tr>
  <tr>
    <td align="center"><b>📊 概览看板</b><br/><sub>用户 / 生成量 / Provider 健康 / 24h 趋势</sub></td>
    <td align="center"><b>🧩 模型管理</b><br/><sub>按模型独立配置能力、定价与权重</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/screenshots/accounts.png" alt="账号池管理" /></td>
    <td width="50%"><img src="docs/screenshots/logs.png" alt="调用日志" /></td>
  </tr>
  <tr>
    <td align="center"><b>🔑 账号池管理</b><br/><sub>多账号池 · 权重 / 并发 · 一键增删改</sub></td>
    <td align="center"><b>📜 调用日志</b><br/><sub>成功 / 失败 / 进行中 · 提示词与耗时全留痕</sub></td>
  </tr>
</table>

## 🚀 核心功能

#### 🎨 生成能力
- 生图 + 生视频一站式,支持**图生图 / 参考图**(首帧、末帧、风格参考)，以及按模型声明的参考视频、参考音频与同步音频生成
- 多分辨率(图像 1K / 2K / 4K · 视频 720p / 1080p)、多宽高比、视频多时长,按模型独立配置与定价
- 9 大供应商、十余模型,后台**动态启用 / 下架 / 改价**,无需改代码
- **模型别名**:同一模型可配多个对外 id,API 调用任意别名均可命中
- **去AI特征**(可选):画图台一键开启,生成图片自动做去AI痕迹处理(细节微扰 + 去除元数据),按画质档位加收积分(默认 1K+1 / 2K+2 / 4K+3,后台可改价、可整体关闭);带标记的作品在画图台、创作记录、日志与后台图片管理中均有「去AI特征」标识

#### 🔌 OpenAI 兼容
- 文本对话 `/v1/chat/completions`(普通 JSON + SSE 流式,兼容 OpenAI SDK)
- 可直接复用后台现有 ChatGPT 账号池并使用真实模型名(`gpt-5-5-mini` / `gpt-5-5-thinking`);Grok 账号既可运行 Web fast 对话,也会按需用 SSO 自动授权 Grok Build OAuth,通过真实 `grok-4.5` 模型对话;还可走自定义 OpenAI 兼容上游
- 文生图 `/v1/images/generations` · 图生图 `/v1/images/edits`(multipart 上传参考图) · 视频 `/v1/videos`(Sora 式异步:创建→轮询→`/content` 下载，支持按模型上传参考视频/音频及生成同步音频) · `/v1/models`(默认返回比例、分辨率、时长、参考素材上限和音频输出等扩展能力；`?extended=false` 可取严格 OpenAI 四字段对象，比例统一为 `W:H`)
- **严格 OpenAI 入参**:图像 `size` 决定比例和原生模型分辨率档(看长边 → 1K/2K/4K);仅 GPT Image 2 家族(`gpt-image-2` / `firefly-gpt-image-2` / `lumina-gpt-image-2`)使用显式 `quality=low/medium/high` 适配为 1K/2K/4K,其他模型忽略它对分辨率的影响;视频 `size` 看短边映射 720p/1080p,改个 `base_url` + `api_key` 即接现有 OpenAI SDK
- 图片结果默认返回 **URL**,普通请求不下载、不做 base64 编码、不留存文件;显式传 `response_format=b64_json` 时才内联图片。携带 `Idempotency-Key` 的请求会把结果保存到账号私有存储,可在网关超时后通过任务查询恢复;站内 **/docs** 附「分辨率对照表」直接查 `size` 该传什么

#### 🔁 多账号池 + 智能故障转移
- 账号池调度,单账号出错不影响整体
- **权重优先 + 并发感知**:按账号权重从高到低调度,某账号并发满了才轮到下一个;同权重组内 round-robin 均摊。每账号并发数可配(上游账号),其余系统固定
- **额度耗尽→换号** · **认证失效→刷新重试 / 判死** · **临时错误→同号重试 ×3** · **Adobe 第三方图片持续过载→兼容供应商降级** · **参数错→直接报错**
- **预扣额度**:生成前原子扣减,失败自动退回,杜绝并发超额

#### 🔗 自定义上游聚合(OpenAI 兼容)
- 把任意 **OpenAI 兼容的 v1 端点**当成一个账号接入(填 `base_url` + `key`),无需写代码
- **按 model id 自动路由**:上游声明支持哪些 id,生成该 id 时即走对应上游(可覆盖内置 provider);id 留空 = 全部
- 模型管理里自由新建自定义模型(id / 文本·图像·视频类型 / 每请求价 / 分辨率·价 / 时长·价 / 多媒体参考能力),按本地价计费
- 自定义上游默认从服务器本机直连；上游可配权重与并发,与内置池统一调度。后台全局代理只用于 ChatGPT、Grok 与 OreateAI 已确认需要住宅出口的受保护控制面，不会无差别转发自定义上游流量

#### 🔐 Token 自动保活
- 一次性轮换 token(Krea / Imagine)**到期前 10 分钟主动续期**,新 token 自动落库
- Adobe cookie 定时换 token;纯 JWT 到期自动判死
- 每日额度按平台重置时间自动恢复 + 重新探测真实余额；OreateAI 低于 60 点保留为限额账号，后台定时刷新并在额度恢复后自动启用

#### 💳 计费与运营
- 积分制(**预扣 + 失败退款**),文本按每请求固定价,图像 / 视频按模型、分辨率、时长精细定价;去AI特征按画质另计附加积分
- **代理价体系**:用户可设为「代理」角色,模型可设代理价;代理用户(含其 API Key 调用)自动按代理价计费,未设代理价则回退普通价
- **在线充值(易支付)**:微信 / 支付宝扫码,金额档位 + 自定义,订单 30 分钟未付自动取消,支付回调 MD5 验签 + 幂等自动到账;累计充值可查
- **CDK 兑换码**充值 · **邀请奖励** · 邮箱注册 / 验证码 / 找回密码
- **并发分组**:按分组限制用户「同时生成数」(画图台 + API Key 合计,`0` = 不限),Redis 自愈计数,新用户自动入默认组
- 三级角色:普通用户 / 代理 / 管理员(唯一)

#### 🖥️ 用户前台(Vue 3)
- 画图台 · 创作记录画廊 · 生成日志(含失败原因 / 来源标签)
- 作品画廊**多选批量操作**:全选本页 / 批量删除(视频连带首尾帧图) / 批量下载(多个自动打包 zip,并发拉取)
- 预览大图内置**复制原图 / 下载 / 关闭**按钮,卡片一键复制原图
- **充值 · 订单**(充值记录 / 未付可继续支付) · API 文档 · API Key 管理 · 邀请 · 关于,亮 / 暗主题
- **站内公告**:登录后自动弹出 Markdown 公告,内容更新即重推

#### 🛠️ 管理后台
- 概览看板(趋势 / DAU / 失败 Top / 消费榜)
- 模型管理(普通价 + 代理价 + 别名) · 账号管理(批量导入 / 去重 / 单账号额度刷新，支持 BytePlus Lumina 完整 Cookie、CPA JSON/ZIP、Sub2API 聚合 JSON 与 grok2api JSON) · **并发分组** · **订单管理**(筛选 / 搜索 / 分页) · 全站日志 · 用户管理(设为代理 / 分配并发组 / 看累计充值 / 违禁触发次数) · CDK · 图片管理(多选批量删除 / zip 打包下载) · 展示位 · **站点公告** · 站点配置(含易支付、去AI特征开关与附加价格)
- **违禁词管理**:后台增删违禁词(分页 + 多选批量删除),提示词命中即拦截(画图台 + API,不区分大小写),按词 / 按用户统计触发次数

**🧰 工程亮点**:tls-client(Chrome JA3/JA4 指纹)稳定穿透 Cloudflare · 媒体存 S3/RustFS 经鉴权代理分发 + 保留期清理 · 自愈式维护轮询(恢复额度 / 刷新凭据 / 清理僵死任务并退款) · 一条命令 Docker 部署(TLS 交给你的反代)。

## 🤖 支持的模型 / 供应商

| 供应商 | 模型(示例) | 类型 |
|---|---|---|
| **Adobe Firefly** | firefly-image-5 · firefly-gpt-image-2 · flux-kontext-max · firefly-video · firefly-ray · gemini-veo31 | 图像 / 视频 |
| **BytePlus Lumina** | lumina-seedream-5.0-pro · lumina-gpt-image-2 · lumina-seedream-5.0-lite · lumina-nano-banana-2 · lumina-nano-banana-pro | 图像（文生图 / 图生图） |
| **OpenAI** | gpt-image-2 | 图像 |
| **Runway** | runway-gen4-turbo · nano-banana-2(Nano Banana 2) | 视频 / 图像 |
| **Grok（Web + Build）** | grok-4.5（SSO 自动换取 Build OAuth） · grok-chat-fast（Web） · grok-video · grok-imagine-image | 文本 / 图像 / 视频 |
| **Leonardo.ai** | seedream-4.5 | 图像 |
| **Krea.ai** | flux-klein-2 | 图像 |
| **Imagine.art** | imagine-1.5 · imagine-1.5pro | 图像 |
| **OreateAI** | oreate-seedance-2.0-mini · oreate-seedance-2.0-fast · oreate-seedance-1.5-pro · oreate-seedance-2.0 · oreate-seedance-2.5 | 视频(文生 / 图片参考 / 视频参考；2.5 支持 5/10/20/30 秒) |
| **自定义上游** | 任意 OpenAI 兼容 v1 端点(按 id 路由) | 文本 / 图像 / 视频 |

> 模型由管理后台动态启用并定价,可随时增删。BytePlus Lumina 内置接入仅开放表中的 5 个 `lumina-*` ID，避免与 OpenAI / Runway 的同名模型冲突；账号须在后台粘贴包含 `csrfToken` 的完整官网 Cookie。Cookie 属于高敏感凭据，请勿写入日志或提交仓库。自定义上游支持把任何 OpenAI 兼容服务接成账号,按 model id 路由调用。

## 🔌 OpenAI 兼容 API

```bash
# 文本对话 —— 非流式;传 "stream":true 即返回标准 SSE 并以 data: [DONE] 结束
curl https://你的域名/v1/chat/completions \
  -H "Authorization: Bearer sk-xxx" -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-5-mini","messages":[{"role":"user","content":"你好"}]}'

# 文生图 —— 原生模型由 size 决定比例和分辨率;仅 GPT Image 2 家族适配 quality
curl https://你的域名/v1/images/generations \
  -H "Authorization: Bearer sk-xxxx" \
  -H "Idempotency-Key: image-request-001" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "a cute cat on a desk, studio lighting",
    "size": "2048x2048"
  }'

# 同步请求遇到 524/504 后按原幂等编号查询,直到 status 为 completed 或 failed
curl 'https://你的域名/v1/images/tasks?request_id=image-request-001' \
  -H "Authorization: Bearer sk-xxxx"

# 可选异步模式 —— 立即返回 HTTP 202、request_id 和 poll_url，适合 Cloudflare 长任务
curl https://你的域名/v1/images/generations \
  -H "Authorization: Bearer sk-xxxx" \
  -H "Prefer: respond-async" \
  -H "Idempotency-Key: image-request-002" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-image-2","prompt":"a cute cat","size":"2048x2048"}'

# 图生图 —— multipart 上传参考图(可多张 image[])
curl https://你的域名/v1/images/edits \
  -H "Authorization: Bearer sk-xxxx" \
  -F model="seedream-4.5" -F prompt="改成赛博朋克风格" -F image=@input.png
```

文本请求原样透传 OpenAI Chat Completions 字段,支持普通 JSON 与 `stream:true` SSE。文本按管理端配置的每请求固定积分收费;上游业务错误、空响应或流未正常结束会记失败并自动退款。图片默认返回 OpenAI 风格 `{ "created": ..., "data": [{ "url": "..." }] }`;普通请求直接返回上游 URL,不下载、不做 base64 编码、不留存。ChatGPT/Grok 等鉴权素材会返回本站 `/content` 流式代理 URL。显式传 `"response_format":"b64_json"` 才返回内联图片。同步图片生成超过 10 秒时会发送 JSON 合法前置空白心跳并关闭 Nginx 响应缓冲，避免下游、反代和 Cloudflare 把仍在工作的请求判为闲置；最终 OpenAI JSON 结构不变，标准 JSON 解析器无需调整。携带 `Idempotency-Key` 时结果保存到账号私有存储,同步响应遇到网关超时后可轮询 `GET /v1/images/tasks?request_id=...` 恢复。发送 `Prefer: respond-async`（或 `?async=true`）则立即返回 `202` 任务对象并通过同一查询接口轮询；未发送该标志的现有下游保持同步响应不变。**视频**走异步:`POST /v1/videos` 建任务 → 轮询 `GET /v1/videos/{id}` 至 `completed` → `GET /v1/videos/{id}/content` 取 mp4。`size` 保持 OpenAI 的 720p/1080p 映射；OreateAI 等带 480p 档位的模型可额外传 `"resolution":"480p"`。OreateAI 1.5/2.0 系列只开放 5/10 秒，Seedance 2.5 另开放 20/30 秒；实时账号配置确认 1.5 Pro 最多两张首尾帧，2.0/2.5 最多 9 张参考图和 3 段参考视频（视频总时长向上取整后须为 2–15 秒），当前不声明参考音频。完整参数见站内 **/docs** 文档页。

## 🚀 部署

> 域名 + HTTPS 由你自己的反向代理处理(本项目不内置证书签发)。

**Docker(推荐)**:`docker compose up -d --build` 一条命令拉起 PostgreSQL + Redis + RustFS + 后端 + 前端(nginx **HTTP 监听容器 2000 端口**),把你的反向代理指到 `http://<本机>:2000`(端口用 `WEB_PORT` 改;要改密码 / 密钥 / `CORS_ORIGINS`(反代走 HTTPS 时把 `COOKIE_SECURE` 设为 `true`),直接改 `docker-compose.yml` 里对应值即可)。后台“全局代理”(`proxy.url`)接受 `http://user:password@proxy:port`、HTTPS 或 SOCKS5 地址，但只注入 ChatGPT、Grok 与 OreateAI：这些平台的登录/账号校验、必要的 bootstrap/challenge、签名和受保护 submit 使用住宅出口；客户端内部已确认安全的参考素材字节上传、状态/成品读取则使用服务器直连。Adobe、Runway、Leonardo、Krea、Imagine 与 Custom 默认全程直连，不消耗住宅代理。OreateAI 的 Chromium 只用于临时浏览器配置中的官网 Banti 签名；上传令牌属于代理控制面，令牌签发后的 Google Storage 素材字节固定直连 `storage.googleapis.com`。清空 `proxy.url` 后 ChatGPT、Grok 与 OreateAI 的代理阶段也恢复直连。该值可能含凭据，请仅授予管理员、不要写入日志，并将代理出口限制为应用服务器。

也可**从源码手动构建**,自备 **PostgreSQL · Redis · RustFS(或任意 S3)· 反向代理**:

```bash
# 1. 创建空库(后端启动自动建表)
createdb vivid_ai

# 2. 配置并从源码构建后端
cat > backend/.env <<'EOF'
APP_ENV=production
HTTP_ADDR=127.0.0.1:6666
POSTGRES_DSN=host=127.0.0.1 user=postgres password=你的密码 dbname=vivid_ai port=5432 sslmode=disable TimeZone=Asia/Shanghai
REDIS_ADDR=127.0.0.1:6379
RUSTFS_ENDPOINT=http://127.0.0.1:9000
RUSTFS_BUCKET=vivid-ai
RUSTFS_ACCESS_KEY=你的AK
RUSTFS_SECRET_KEY=你的SK
CORS_ORIGINS=https://你的域名
COOKIE_SECURE=true
EOF
cd backend && go build -o bin/api ./cmd/api && ./bin/api   # 监听 127.0.0.1:6666

# 3. 构建前端(产物 frontend/dist)
cd frontend && npm install && npm run build
```

Nginx 反代(证书自行用 certbot / acme.sh):

```nginx
server {
    listen 443 ssl;
    server_name 你的域名;
    ssl_certificate     /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;
    root /path/to/frontend/dist;
    index index.html;
    client_max_body_size 320m;
    proxy_read_timeout 600s;            # 视频生成耗时长

    location /assets/ { expires 1y; add_header Cache-Control "public, max-age=31536000, immutable"; }
    location / { try_files $uri $uri/ /index.html; add_header Cache-Control "no-cache"; }
    location ^~ /admin/api/ { proxy_pass http://127.0.0.1:6666; }
    location ^~ /images/    { proxy_pass http://127.0.0.1:6666; }
    location = /health      { proxy_pass http://127.0.0.1:6666; }
    location ^~ /v1/        { proxy_pass http://127.0.0.1:6666; add_header Cache-Control "no-store" always; }
}
```

> 完整环境变量见 `backend/.env.example`。

## 🧱 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go · gin · gorm(PostgreSQL)· go-redis · tls-client(Chrome 指纹) |
| 前端 | Vue 3 · Vue Router · Vite · Tailwind CSS v4 |
| 基础设施 | PostgreSQL · Redis · RustFS(S3 兼容)· Nginx |

## 📦 仓库结构

```
backend/                       后端源码(Go)
├── cmd/
│   ├── api/                   服务入口(main)
│   └── marklabel/             运维小工具(按需标记账号)
├── internal/
│   ├── bootstrap/             应用装配、定时维护任务启动
│   ├── config/                环境变量配置加载
│   ├── http/
│   │   ├── handler/           HTTP 处理器(v1 兼容接口、后台、鉴权…)
│   │   ├── middleware/        鉴权 / 请求 ID 等中间件
│   │   └── router/            路由注册
│   ├── model/                 GORM 数据模型
│   ├── provider/              各上游供应商客户端
│   │   ├── adobe/             Adobe Firefly(tls-client 指纹)
│   │   ├── byteplus/          BytePlus Lumina 官网五模型（Cookie + ImageX）
│   │   ├── chatgpt/           OpenAI(含 PoW / turnstile)
│   │   ├── runway/            Runway 视频 + Nano Banana 图像
│   │   ├── grok/              Grok Web(statsig) + SSO→Build OAuth(grok-4.5)
│   │   ├── leonardo/          Leonardo
│   │   ├── krea/              Krea
│   │   ├── imagine/           Imagine.art
│   │   ├── oreate/            OreateAI Seedance(官网 Banti 签名)
│   │   ├── custom/            自定义上游(OpenAI 兼容 v1,按 id 路由,默认直连)
│   │   └── epay/              易支付(mapi 下单 + 回调 MD5 验签,积分充值)
│   ├── repo/                  数据访问层(用户 / 模型 / 账号 / 日志 / CDK / 订单 / 并发组…)
│   ├── service/              业务逻辑(生成调度、计费、账号池、保活、维护)
│   └── storage/               RustFS / S3 媒体存储
├── Dockerfile                 多阶段构建(源码编译 → 精简运行镜像)
└── .env.example               后端环境变量模板

frontend/                      前端源码(Vue 3 + Vite)
├── src/
│   ├── views/                 页面(画图台 / 账号 / 模型 / 用户 / 并发组 / 订单 / 日志 / 概览 / 充值 / 设置…)
│   ├── components/             复用组件(弹窗 / 选择器 / 灯箱…)
│   ├── layouts/                公共 / 后台布局
│   ├── utils/                  工具函数
│   └── api.js · auth.js …      接口封装、鉴权、主题、积分等
├── Dockerfile                 Nginx 静态托管(HTTP :2000)+ API 反代
└── default.conf.template      Nginx 站点模板(反代 + 缓存策略)

docker-compose.yml             Docker 编排(Postgres / Redis / RustFS / 后端 / 前端)
.env.example → backend/.env    后端环境变量模板
```

## 🗺️ Roadmap

- [ ] 更多上游供应商接入
- [ ] 用量统计 / 导出
- [ ] 多语言界面(i18n)
- [ ] Webhook / 异步回调

## 💬 交流 / 联系

| | |
|---|---|
| 🌐 官网 | **[vividai.run](https://vividai.run)** |
| 👥 QQ 交流群 | **1106849765** · [点击加群](https://qm.qq.com/q/976LeMFoHu) |
| 🐧 QQ | **1114639355** · [加好友](https://qm.qq.com/q/ItgCcNA7ac) |
| 🛒 小店 | **[pay.ldxp.cn/shop/chiyi](https://pay.ldxp.cn/shop/chiyi)** |
| ✉️ 邮箱 | vividairun@gmail.com |

## ⭐ Star History

<!-- 建好 GitHub 仓库后,取消下面这行注释并把 OWNER 换成你的用户名即可显示趋势图: -->
<!-- [![Star History Chart](https://api.star-history.com/svg?repos=OWNER/image2api&type=Date)](https://star-history.com/#OWNER/image2api&Date) -->

觉得有用就点个 ⭐ 吧 —— 建好仓库后取消上方注释即可展示 Star 趋势图。

## 📄 License

本项目(前端 + 后端)基于 [MIT](LICENSE) 协议开源,可自由使用、修改、商用与二次分发。

<div align="center">

如果这个项目对你有帮助,欢迎 ⭐ Star 支持!

</div>
