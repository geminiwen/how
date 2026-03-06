# HTTP Features in HOWS Protocol

本文档说明 HOWS 协议中 HTTP 请求/响应的支持范围和限制。

由于 HTTP 请求和响应被序列化为 MessagePack 消息通过 WebSocket 传输，并非所有 HTTP 特性都能被完整支持。

## 支持的特性

### 基本 HTTP 语义
- **所有 HTTP 方法**: GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS 等
- **状态码**: 所有标准 HTTP 状态码（1xx-5xx）
- **请求头和响应头**: 完整支持多值头（同一 key 多个 value）
- **请求体和响应体**: 作为完整的字节数组传输（非流式模式）
- **查询参数**: 保留在 URL 中原样传输
- **路径**: 完整保留（包括路径参数、百分号编码等）
- **多路复用**: 同一 WebSocket 连接上通过 request_id 实现多请求并发

### 流式响应（v0.2.0 新增）
- **Server-Sent Events (SSE)**: 支持。通过 `HTTPResponseStart` + `HTTPResponseChunk` + `HTTPResponseEnd` 消息序列实现。
- **流式响应体**: 支持。服务端每收到一个 chunk 立即 flush 给上游调用者。
- **活动超时**: 流式响应使用活动超时（每收到一个 chunk 重置计时器），而非绝对超时。

### 头部处理
- **自定义头部**: 完整传输
- **标准头部**: Content-Type, Authorization, Accept 等如实传输
- **代理头部**: 自动添加 X-Forwarded-For

## 不支持的特性

### 请求体流式传输
- **Chunked 请求体**: 不支持。请求体必须完整读取后才能传输。
- 注意：响应体流式传输已支持（见上方），但请求体仍需完整缓冲。

### 连接级特性
- **HTTP/2 Server Push**: 不支持。
- **WebSocket 升级**: 不支持在 HOWS 隧道内再嵌套 WebSocket 连接。
- **Keep-Alive / 持久连接**: 不适用。每个请求是独立的消息对。
- **Connection 头部**: hop-by-hop 头部应被忽略。
- **Upgrade 头部**: 不支持协议升级。

### 1xx 状态码
- **100 Continue**: 不支持 `Expect: 100-continue` 机制。请求体总是随请求一起发送。
- **101 Switching Protocols**: 不支持。
- **103 Early Hints**: 不支持。

### Trailer Headers
- **HTTP Trailers**: 不支持。只传输常规头部。

### 内容编码
- **Transfer-Encoding**: 不适用。消息体作为原始字节传输。
- **Content-Encoding (gzip, br 等)**: 透传。HOWS 本身不执行压缩/解压操作。

### 大文件
- **非流式模式**: 请求体/响应体需放入单条消息，建议限制 10 MB。
- **流式模式**: 响应体可分块传输，无总大小限制（每个 chunk 建议不超过 64 KB）。
- **请求体**: 始终需完整缓冲，不适合超大文件上传。

### 认证相关
- **NTLM 认证**: 不支持（依赖连接级状态）。
- **HTTP 摘要认证 (Digest Auth)**: 头部透传，但多轮质询需上层自行处理。

### 缓存
- **条件请求**: 头部如实传输，但 HOWS 不执行缓存逻辑。

### 其他
- **Cookies**: 头部如实传输，HOWS 不维护 cookie jar。
- **重定向**: 头部如实传输，HOWS 不自动跟随重定向。
- **Range 请求**: 头部如实传输，非流式模式下部分内容响应仍需完整缓冲。
- **Multipart 表单**: 作为原始字节传输，受非流式模式消息大小限制。

## 已知行为差异

| 场景 | 标准 HTTP | HOWS |
|------|----------|------|
| 请求体传输 | 可以流式传输 | 必须完整缓冲 |
| 响应体传输 | 可以流式传输 | 支持流式（v0.2.0）或完整缓冲 |
| SSE | 原生支持 | 通过流式响应消息支持 |
| 连接超时 | TCP 级别超时 | 应用级超时（默认 60s，流式模式按活动重置） |
| 并发请求 | 每连接一个（HTTP/1.1）或多路复用（HTTP/2） | 单 WebSocket 连接上通过 request_id 多路复用 |
| 最大消息大小 | 理论无限制 | 非流式：建议 10 MB；流式：每 chunk 建议 64 KB |
| TLS | 端到端 | 分段：上游→服务器 和 服务器↔客户端 各自独立 |

## 未来可能的扩展

1. **请求体分块传输**: 引入 `HTTPRequestChunk` 消息类型，支持大文件上传。
2. **协议层压缩**: 对消息体进行压缩（需要协商压缩算法）。
