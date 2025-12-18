# GPT-Load 渠道（OpenAI / Gemini / Claude）特征与接入说明

本文档面向使用/二开 GPT-Load 的同学，说明三种常用渠道类型（`openai` / `gemini` / `anthropic`(Claude)）在 **请求路径、认证方式、典型 payload、流式判断、模型字段位置、模型列表/重定向** 等方面的差异，以及 GPT-Load 在转发链路中做了哪些处理。

> 说明：GPT-Load 是透明代理，**不会对业务 payload 做强校验**，大部分字段完全透传；渠道差异主要体现在 **鉴权注入**、**流式识别**、**模型提取/重定向** 与 **模型列表响应转换**。

---

## 1. 基础概念

### 1.1 渠道（Channel Type）与分组（Group）

- **分组（Group）**：你在管理界面创建的路由单元（例如 `openai`、`gemini`、`anthropic`）。每个分组包含：
  - 上游地址列表 `upstreams`（支持权重）
  - 上游 API Key 池（用于轮询/熔断/重试）
  - `channel_type`（决定鉴权注入方式、模型处理方式等）
  - 可选：`proxy_keys`（分组级代理访问密钥）、`header_rules`、`param_overrides`、`model_redirect_rules`
- **渠道类型（channel_type）**：目前代码内置四种：
  - `openai`（OpenAI / OpenAI-compatible）
  - `gemini`（Google Gemini：原生 REST +（可选）OpenAI-compatible 路径）
  - `vertex_gemini`（Google Vertex AI Gemini：原生 REST（payload 与 Gemini 原生一致），鉴权为 OAuth2 access token）
  - `anthropic`（Anthropic Claude：Messages API）

### 1.2 代理端点与路径映射

代理访问格式固定为：

```text
http(s)://{proxy_host}/proxy/{group_name}/{upstream_original_path}?{query}
```

GPT-Load 的路径映射规则（简化描述）：

- 取出请求路径中的前缀 `/proxy/{group_name}`，剩余部分保持不变
- 将剩余路径拼到分组配置的上游 base URL 后面
- **Query string 原样透传**（但代理鉴权用到的 `key` 会在鉴权阶段被移除，见下文）

例如：

- 客户端请求：`/proxy/openai/v1/chat/completions`
- 上游 base：`https://api.openai.com`
- 上游最终：`https://api.openai.com/v1/chat/completions`

### 1.3 代理自身的身份验证（Proxy Keys）

GPT-Load 的 **代理鉴权**（用于保护 `/proxy/...`）支持从以下位置提取 “代理访问密钥”：

1. URL Query：`?key=...`（会从 query 中移除后再转发，避免泄漏给上游）
2. Header：`Authorization: Bearer ...`
3. Header：`X-Api-Key: ...`
4. Header：`X-Goog-Api-Key: ...`

校验逻辑：

- 先查 **系统全局**配置的 `proxy_keys`
- 再查 **分组级**配置的 `proxy_keys`
- 命中任意一个即放行

> 实践建议：为了跨渠道统一，手写调用/自研客户端可优先使用 `Authorization: Bearer {proxy_key}`；使用官方 SDK 时，按 SDK 支持的方式传入即可（OpenAI 通常是 Bearer，Gemini 通常是 query `key`，Anthropic 通常是 `x-api-key`）。

---

## 2. GPT-Load 转发链路的“通用行为”

无论是哪种渠道，GPT-Load 对转发请求做的通用处理包括：

### 2.1 客户端鉴权信息清理（防止透传到上游）

在构造上游请求时，GPT-Load 会删除客户端请求中的这些 header（避免把你的 **代理 key** 误传给上游）：

- `Authorization`
- `X-Api-Key`
- `X-Goog-Api-Key`

随后由各渠道按自己的规则写入 **上游真实 key**。

### 2.2 Header Rules（可选）

分组可配置 `header_rules` 来对上游请求头做二次处理（`set` / `remove`）。

典型用途：

- 补充/覆盖厂商要求的版本头、项目头
- 对接兼容网关时设置自定义鉴权头

### 2.3 Param Overrides（可选）

分组可配置 `param_overrides`（JSON map），对 **JSON 请求体** 做强制覆盖/补充：

- 仅当请求体可解析为 JSON object 时生效
- 仅作用于 body 内的 key-value

> 注意：Gemini **原生**接口的模型通常在 URL path 上（`.../models/{model}:...`），`param_overrides` 无法改写 path 中的模型名；需要用 `model_redirect_rules` 来做模型重定向（见后文）。

### 2.4 Model Redirect（可选）

分组可配置 `model_redirect_rules`（source->target 映射）与 `model_redirect_strict`（严格模式）：

- **OpenAI / Anthropic**：重定向通过修改 JSON body 里的 `model` 字段完成
- **Gemini 原生**：重定向通过修改 URL path 中 `models/{model}` 段完成
- **严格模式**：如果请求的模型不在重定向规则里，直接返回 `400`

### 2.5 Model List 拦截与转换（可选）

当请求满足以下条件时（GET 且命中常见模型列表路径），GPT-Load 会拦截并对响应做“模型列表过滤/合并”：

- `.../v1/models`（OpenAI / OpenAI-compatible）
- `.../v1beta/models`（Gemini 原生）
- `.../v1beta/openai/v1/models`（Gemini OpenAI-compatible）

转换规则取决于 `model_redirect_rules` 和 `model_redirect_strict`：

- 严格模式：仅返回配置过的 source 模型（白名单）
- 非严格：返回“上游列表 + 配置模型”，上游优先去重合并
- Gemini 原生列表存在分页：仅在 **第一页**（无 `pageToken`）做合并，避免翻页重复注入

### 2.6 流式请求识别（用于选择 StreamClient + 返回 SSE）

GPT-Load 会根据渠道规则判断是否为流式请求，然后：

- 使用专用的 streaming HTTP client（无请求超时、禁压缩、更大的连接池）
- 将响应以流的方式转发给客户端，并强制设置：
  - `Content-Type: text/event-stream`
  - `Cache-Control: no-cache`
  - `Connection: keep-alive`
  - `X-Accel-Buffering: no`

---

## 3. `openai` 渠道

### 3.1 上游地址与典型路径

常见上游 base URL：

- `https://api.openai.com`
- 或任意 OpenAI-compatible 网关/自建服务

典型路径（示例）：

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `POST /v1/embeddings`
- `GET  /v1/models`
- 以及其它 OpenAI 兼容路径（GPT-Load 做透明转发）

### 3.2 身份验证（上游）

上游鉴权写入方式：

- `Authorization: Bearer {upstream_api_key}`

客户端访问 GPT-Load 时：

- 你仍然以 OpenAI 的方式携带 **代理 key**（Bearer），GPT-Load 会在转发前清理并替换为上游真实 key。

### 3.3 典型 payload（示例）

GPT-Load 不改写 OpenAI payload（除非启用了 `param_overrides`/`model_redirect_rules`）。

Chat Completions（非流式）常见结构：

```json
{
  "model": "gpt-4.1-mini",
  "messages": [
    {"role": "user", "content": "Hello"}
  ]
}
```

流式时通常增加：

```json
{"stream": true}
```

> 其余字段（例如 `temperature`、`top_p`、`tools`、`response_format` 等）完全由上游定义并透传。

### 3.4 流式识别规则

满足任意一个即视为流式：

- `Accept` 包含 `text/event-stream`
- query 参数 `stream=true`
- JSON body 中 `{"stream": true}`

### 3.5 模型字段位置与重定向

- 模型字段：JSON body 的 `model`
- 重定向方式：改写 `model` 为映射后的目标模型

### 3.6 Key 校验（Key Validation）

用于后台定期验证 key 是否可用的探活请求（默认）：

- `POST /v1/chat/completions`
- payload（最小化）：
  - `{"model": "{test_model}", "messages": [{"role":"user","content":"hi"}]}`
- 任何 `2xx` 视为有效

> 可通过分组的 `validation_endpoint` 覆盖默认探活路径。

---

## 4. `gemini` 渠道

Gemini 渠道在 GPT-Load 中同时兼容两类 API 形态：

1. **Gemini 原生 REST**（`/v1beta/models/...:generateContent`）
2. **Gemini OpenAI-compatible**（路径包含 `v1beta/openai` 的变体）

### 4.1 上游地址与典型路径

常见上游 base URL：

- `https://generativelanguage.googleapis.com`

原生 REST 常见路径：

- `POST /v1beta/models/{model}:generateContent`
- `POST /v1beta/models/{model}:streamGenerateContent`
- `GET  /v1beta/models`（模型列表，可能分页）

OpenAI-compatible 常见路径（取决于上游网关实现）：

- `POST /v1beta/openai/v1/chat/completions`
- `GET  /v1beta/openai/v1/models`

### 4.2 身份验证（上游）

GPT-Load 对 Gemini 的鉴权注入分两种情况：

#### A) 原生 REST（默认）

- 将上游 key 写入 query：`?key={upstream_api_key}`

客户端访问 GPT-Load 时：

- 可使用 query `key={proxy_key}`（与原生方式一致）
- 也可用 `Authorization: Bearer {proxy_key}`（代理鉴权同样认可）

#### B) OpenAI-compatible 路径（`v1beta/openai`）

当前实现会将上游 key 写入：

- `Authorization: Bearer {upstream_api_key}`

> 如果你的上游实现要求用 `key=` 或 `X-Goog-Api-Key`，可以用 `header_rules` 做适配（例如移除/设置相关 header），或调整调用方式与上游网关配置保持一致。

### 4.3 典型 payload（示例）

#### A) 原生 REST：`generateContent`

```json
{
  "contents": [
    {
      "role": "user",
      "parts": [{"text": "Hello"}]
    }
  ]
}
```

#### B) OpenAI-compatible：`chat/completions`

与 OpenAI 一致（示例）：

```json
{
  "model": "gemini-2.5-pro",
  "messages": [{"role": "user", "content": "Hello"}]
}
```

### 4.4 流式识别规则

满足任意一个即视为流式：

- 请求 path 以 `:streamGenerateContent` 结尾（Gemini 原生）
- 或作为兜底：`Accept: text/event-stream` / `stream=true` / body `{"stream": true}`

### 4.5 模型字段位置与重定向

- 原生 REST：模型在 URL path 中 `.../models/{model}:...`
  - 重定向：改写 URL path 中的 `{model}` 段
- OpenAI-compatible：模型在 JSON body 的 `model`
  - 重定向：改写 body 的 `model`

严格模式（`model_redirect_strict=true`）：

- 请求模型不在 `model_redirect_rules` 里则直接 `400`

### 4.6 模型列表（`/v1beta/models`）的响应结构差异

Gemini 原生模型列表一般形如：

```json
{
  "models": [...],
  "nextPageToken": "..."
}
```

因此 GPT-Load 在 Gemini 渠道里会：

- 识别 `models` 字段并按 Gemini 结构合并/过滤
- 严格模式下只返回配置模型，并移除 `nextPageToken`
- 非严格模式下仅在第一页做“上游 + 配置”合并

### 4.7 Key 校验（Key Validation）

用于后台验证 key 的探活请求（固定使用原生 REST）：

- `POST /v1beta/models/{test_model}:generateContent?key={upstream_api_key}`
- payload（最小化）：
  - `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
- 任何 `2xx` 视为有效

> 当前实现不会读取分组的 `validation_endpoint` 来构造 Gemini 探活请求；如果你只使用 OpenAI-compatible 路径且上游鉴权不是 API key 形态，需要留意这一点。

---

## 5. `vertex_gemini` 渠道（Vertex AI Gemini）

Vertex AI Gemini 渠道用于对接 **Vertex AI 上的 Gemini 模型**。其 **payload 与 Gemini 原生 REST 一致**，主要差异在于：

- 上游请求路径是 Vertex AI 形态（带 `project_id` / `location`）
- 上游鉴权是 **OAuth2 access token**（由 GCP Service Account JSON 换取并注入）

### 5.1 上游地址与典型路径

常见上游 base URL（与 `location` 绑定）：

- `https://{location}-aiplatform.googleapis.com`（例如 `https://us-central1-aiplatform.googleapis.com`）
- `https://aiplatform.googleapis.com`（当 `location=global`）

典型路径（示例）：

- `POST /v1/projects/{project_id}/locations/{location}/publishers/google/models/{model}:generateContent`
- `POST /v1/projects/{project_id}/locations/{location}/publishers/google/models/{model}:streamGenerateContent`
- `GET  /v1/projects/{project_id}/locations/{location}/publishers/google/models`

兼容说明（可选）：

- 若客户端仍按 Gemini 原生方式请求（`/v1beta/models/...` 或 `/v1/models/...`），当前实现会在转发到上游前自动改写为 Vertex AI 路径：
  - `project_id` 来自导入的 Service Account JSON
  - `location` 从分组的上游 URL（host/path）推断（因此天然是“按分组绑定地区”）

### 5.2 身份验证（上游）

Vertex AI Gemini 上游鉴权写入方式：

- `Authorization: Bearer {access_token}`

其中 `access_token` 来自分组 key 池里导入的 **GCP Service Account JSON**（JWT Bearer -> token_uri），并会在有效期内缓存复用。

> Key 导入建议：直接导入/粘贴 **Service Account JSON 的原始内容**（单个 JSON object 或 JSON array），由系统加密存储；不建议仅保存服务器上的文件路径（多实例/容器场景不可靠）。

### 5.3 典型 payload（示例）

与 Gemini 原生 REST 一致（示例）：

```json
{
  "contents": [
    {
      "role": "user",
      "parts": [{"text": "Hello"}]
    }
  ]
}
```

### 5.4 流式识别规则

与 Gemini 原生一致，满足任意一个即视为流式：

- 请求 path 以 `:streamGenerateContent` 结尾
- 或作为兜底：`Accept: text/event-stream` / `stream=true` / body `{"stream": true}`

### 5.5 模型字段位置与重定向

- 模型在 URL path 中 `.../models/{model}:...`
- 重定向：改写 URL path 中的 `{model}` 段（规则与 `gemini` 原生一致）

### 5.6 Key 校验（Key Validation）

用于后台验证 key 的探活请求（默认）：

- `POST /v1/projects/{project_id}/locations/{location}/publishers/google/models/{test_model}:generateContent`
- Header：`Authorization: Bearer {access_token}`
- payload（最小化）：
  - `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
- 任何 `2xx` 视为有效

---

## 6. `anthropic` 渠道（Claude）

### 6.1 上游地址与典型路径

常见上游 base URL：

- `https://api.anthropic.com`

典型路径：

- `POST /v1/messages`
- `GET  /v1/models`（如上游支持）

### 6.2 身份验证（上游）

GPT-Load 写入 Anthropic 上游鉴权头：

- `x-api-key: {upstream_api_key}`
- `anthropic-version: 2023-06-01`（当前实现固定写入）

客户端访问 GPT-Load 时：

- 可按 Anthropic 原生方式携带 **代理 key**（`x-api-key`）
- 也可用 `Authorization: Bearer {proxy_key}`（代理鉴权同样认可）

### 6.3 典型 payload（示例）

Anthropic Messages API 常见结构（非流式）：

```json
{
  "model": "claude-sonnet-4-20250514",
  "max_tokens": 256,
  "messages": [
    {"role": "user", "content": "Hello"}
  ]
}
```

流式时通常增加：

```json
{"stream": true}
```

> 其余字段（例如 `system`、`tools`、`temperature` 等）由上游定义并透传。

### 6.4 流式识别规则

与 OpenAI 相同，满足任意一个即视为流式：

- `Accept` 包含 `text/event-stream`
- query 参数 `stream=true`
- JSON body 中 `{"stream": true}`

### 6.5 模型字段位置与重定向

- 模型字段：JSON body 的 `model`
- 重定向方式：改写 `model` 为映射后的目标模型

### 6.6 Key 校验（Key Validation）

用于后台验证 key 的探活请求（默认）：

- `POST /v1/messages`
- payload（最小化）：
  - `{"model":"{test_model}","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
- 任何 `2xx` 视为有效

> 可通过分组的 `validation_endpoint` 覆盖默认探活路径。

---

## 7. 四种渠道差异对比（速查）

| 渠道类型 | 常见上游 base URL | 上游鉴权写入 | 模型位置 | 流式判定关键点 | 典型模型列表结构 |
| --- | --- | --- | --- | --- | --- |
| `openai` | `https://api.openai.com` | `Authorization: Bearer ...` | body `model` | `Accept: text/event-stream` / `stream=true` / body `stream` | `{"data":[...]}` |
| `gemini` | `https://generativelanguage.googleapis.com` | 原生：query `key=...`；OpenAI-compatible：`Authorization: Bearer ...` | 原生：path `models/{model}`；兼容：body `model` | 原生：`...:streamGenerateContent` | 原生：`{"models":[...], "nextPageToken":...}`；兼容：`{"data":[...]}` |
| `vertex_gemini` | `https://{location}-aiplatform.googleapis.com` | `Authorization: Bearer {access_token}`（SA JSON 换取） | path `.../models/{model}` | 原生：`...:streamGenerateContent` | 通常：`{"models":[...], "nextPageToken":...}` |
| `anthropic` | `https://api.anthropic.com` | `x-api-key: ...` + `anthropic-version: 2023-06-01` | body `model` | 同 OpenAI | 取决于上游（代码按 `data` 格式处理） |

---

## 8. 常见落坑点与建议

1. **Gemini 两种形态混用**：原生的模型在 URL 上，兼容模式的模型在 body 上；请确认你启用的 `model_redirect_rules` 能覆盖你调用的那种形态。
2. **Gemini 探活固定走原生 `generateContent`**：如果你的上游只暴露 OpenAI-compatible 路径，或鉴权不是 API key 形态，可能需要额外适配（例如自定义渠道/调整实现）。
3. **Vertex Gemini 的地区与权限**：确保分组上游 URL 的 `location` 与实际调用一致（例如 `us-central1-aiplatform.googleapis.com`），并确认 Service Account 有调用 Vertex AI 的权限（且项目已启用相关 API）。
4. **流式响应 Content-Type**：GPT-Load 对流式响应会强制设置为 `text/event-stream`；客户端应按 SSE 方式消费。
5. **模型列表拦截**：GET 模型列表会被代理转换（用于配合模型重定向/严格模式）；如果你希望完全透传模型列表，可不要启用 `model_redirect_rules`/严格模式，或在上游侧做一致化。
6. **统一传 proxy key 的方式**：自研/脚本建议统一用 `Authorization: Bearer {proxy_key}`；SDK 场景按 SDK 的 key 注入方式即可。
