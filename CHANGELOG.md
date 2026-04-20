# Changelog

## 0.2.0 — 未发布

本版本是 `0.1.x` 之后的第一次 breaking change,**协议 wire format 不变**(老/新双端依然能互通),但 TypeScript 和 Go 两端的 API 都做了调整。下面按 "这次想做什么、动了什么、调用方要怎么改" 来写。Wire / 消息字节不变。

---

### 1. Caller:`request()` 返回的 body 永远是 stream

**为什么改**:老 API 里,如果对端是流式响应,`resp.body` 运行时是 `ReadableStream` (TS) / 一次性累积完的 `[]byte` (Go) —— 两个行为不对称,而且 TS 类型声明其实是撒谎(标的是 `Uint8Array` 实际是 stream)。

**改完怎样**:不论对端发的是单个 `HTTPResponse` 还是流式的 `HTTPResponseStart + Chunk* + End`,**调用方看到的都是同一种形态**:

- **TypeScript**
  ```ts
  const resp = await caller.request(req);
  // resp.body 恒为 ReadableStream<Uint8Array>
  ```
  老代码 `new TextDecoder().decode(resp.body)` 失效。要么用 stream 读:
  ```ts
  const text = await new Response(resp.body).text();
  ```
  要么流式消费。

- **Go**
  ```go
  resp, err := caller.Request(ctx, req)
  // resp.Body 恒为 io.ReadCloser
  defer resp.Body.Close()
  ```
  老代码 `string(resp.Body)` 失效。要么:
  ```go
  body, _ := io.ReadAll(resp.Body)
  resp.Body.Close()
  ```
  要么流式消费。

**Go 侧的副作用:能真正流式读了**。之前 Go caller 会把所有 chunk 攒到 End 才返回 `[]byte`,流失去意义。现在第一个 `Start` 到达就 resolve,`Body.Read` 直接读 `io.Pipe`,chunk 边来边可读 —— 典型用法是把它 `c.Response.SetBodyStream(resp.Body, -1)` pipe 给 Hertz 下游。

---

### 2. Handler:可选流式模式(`HTTPHandlerAdapter`)

**为什么改**:老的 `HTTPHandlerAdapter` 把 handler 的全部响应 buffer 完才发一个 `HTTPResponse`。对 SSE / 大文件这种场景 caller 只能等响应完全写完才拿到数据 —— 失去了协议层的流式能力。

**改完怎样**:**默认还是 buffer**(不破坏老代码),想流式就显式 opt-in。

- **TypeScript**:
  ```ts
  createHOWHandler(app, sender, { streaming: true });
  ```
  `streaming` 默认 `false`。开启后,handler 里每写一段,对端都能增量读到。**注意**:底层是 Node HTTP stack 走 loopback socket,chunk 边界不保证 1:1 对应 `res.write()` —— 相邻 write 可能被 TCP 层合并成一个 chunk。做消息分帧要用 in-band delimiter(SSE 的 `\n\n` 就是天然的)。

- **Go**:
  ```go
  client.HTTPHandler(handler, client.WithStreaming())
  ```
  每次 `http.ResponseWriter.Write(p)` 恰好对应一个 `HTTPResponseChunk`(Go 端用的是自定义 `http.ResponseWriter`,直接转发,不走 loopback)。`http.Flusher` 正确实现:第一次 Flush 会提交 header(模仿 `net/http` 的隐式 `WriteHeader(200)`),SSE handler 先 Flush 再 Write 的常见 pattern 可以工作。

`ForwardHandler`(传字符串 URL)两端一直都是流式,行为不变。

---

### 3. 断线感知

**Caller 侧**:加了 `close(err?)` (TS) / `Close(err)` (Go)。WebSocket 被对端挂掉的时候,调用方需要在 `ws.on("close"/"error")` 里调一下这个方法 —— 所有还在 pending 的请求会被立刻 reject,流式 body 会吐 error。不调的话老行为:等 `readTimeout` 超时(默认 30s)才失败。

**Handler 侧 — "写不了就退出"**:`client.ResponseWriter.Write(data []byte) error` 现在**返回 error**(老签名是 void,这是 breaking)。对端 ws 挂了 / send 失败,Write 就返回 err,handler 自己 check 完 return 即可。符合 Go `http.ResponseWriter.Write` 的 `(int, error)` 惯例。`streamingHTTPHandlerAdapter`(`WithStreaming()`)内部已经把这个 err 转成 `http.ResponseWriter.Write` 的 `(0, err)` 返回,业务的 `fmt.Fprintf(w, ...)` 会自动拿到 err 停写。`ForwardHandler.ServeHTTPOverWSStream` 的 pump 循环检到 Write err 也会退出。

**Handler 侧 — ctx 感知**:`WithStreaming()` adapter 会把 HOW dispatch ctx 透传给 wrapped `http.Request`,handler 里 `r.Context().Done()` 能感知到 caller 侧取消 / transport 死 → 业务可以主动退出 SSE 循环。

**Handler 侧主动报失败**:如果 handler 在已发头部之后想让 caller 看到 `Body.Read` 抛错(而不是干净 EOF),**必须实现 `StreamingHandler` 接口然后 `return err`** —— dispatch 会发 `Error` envelope。包装普通 `http.Handler` 的 adapter 无法区分"handler 为何返回"(ctx cancel / panic / 主动 return 看起来一样),所以只会发 `HTTPResponseEnd`。这是 adapter 层的语义盲区,不是 bug —— 写不了就 log 退出,没必要强行猜"handler 是不是中途被打断了"。

---

### 4. Caller 多路复用下的 backpressure

(内部实现细节,API 没变)

老实现里 Caller 给每个 in-flight 请求维护一个 buffered `chan`(大小 16),transport 读循环 `select case ch<-env: default: log.drop`。两个问题:
1. Non-blocking send 在 channel 满时直接丢消息(chunk/End/Error 都可能丢)
2. 如果改成 blocking send,一个慢 reader 会 block 整条 WebSocket 读循环,其他并发请求全 HOL 阻塞

新实现用 per-request 的 **unbounded FIFO**(`frameQueue` + `sync.Mutex` + buffered(1) signal channel):
- `push`: O(1) 非阻塞,transport 读循环永远不 stall
- `wait`: 消费端阻塞 select(sig / done / ctxDone / timerC)
- 单个慢 stream 只会让它自己的 queue 在内存里堆,不会影响其他请求

**已知 trade-off**:单请求的 queue 理论上可以无限长(调用方不读 Body 又不 Close)—— 用户主动 `Body.Close()` 可以触发 pump 退出和 pending 清理。

---

### 5. Spec

- 新增 §7.5 "Caller Body Semantics":规定 caller 对调用方永远暴露 body 为 stream。

---

### 迁移指引

| 场景 | 老代码 | 新代码 |
|---|---|---|
| TS: caller 拿完整 body | `decode(resp.body)` | `await new Response(resp.body).text()` |
| Go: caller 拿完整 body | `string(resp.Body)` | `body, _ := io.ReadAll(resp.Body)` |
| TS: ws 断线清理 | (无) | `ws.on("close", () => caller.close(err))` |
| Go: ws 断线清理 | (无) | `conn.Read 出错时 caller.Close(err)` |
| TS: 让 express handler 流式回传 | buffered `res.end(body)` | `createHOWHandler(app, sender, { streaming: true })` + `res.write(chunk); ...; res.end()` |
| Go: 让 http.Handler 流式回传 | buffered | `HTTPHandler(h, WithStreaming())` + `w.Write(chunk); flusher.Flush()` |
| Go: 实现 StreamingHandler 的代码 | `rw.Write(data)` (void) | `if err := rw.Write(data); err != nil { return err }` |
