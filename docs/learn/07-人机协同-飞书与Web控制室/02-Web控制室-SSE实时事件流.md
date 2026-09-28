# Web 控制室：SSE 实时事件流

> 所属：[亮点七 · 人机协同——飞书与 Web 控制室](README.md)

## 一句话

Web 控制室是一个 React 单页应用，打包后嵌进 Go 二进制。身份完全由服务端判定：机器 Token、个人 Bearer Token，或登录后签发的签名会话 Cookie。权限分 viewer、operator、admin 三级。incident 的实时事件流用 SSE 推送，数据源是数据库：按事件 ID 增量读取，断线后用 Last-Event-ID 续传。

## 页面组成

| 页面 | 内容 |
|---|---|
| 登录（Login） | 输入个人 Token，换取会话 |
| 概览（Overview） | 触发中事件、待审批、自动处置状态、30 天无人介入恢复率 |
| 事件列表（Incidents） | 按状态筛选、按标题过滤，选择要查看的 incident |
| 控制室（IncidentDetail） | 诊断报告、处理流程（8 个阶段的进度）、事件时间线（实时）/ 诊断轨迹（每一步输入输出）/ 问 Agent 三个标签，右侧审批、最近变更、当前问题、告警成员、复盘标注；页头有重新诊断 |
| 处置规则 / 效果评估 / 发布记录 | 规则与阻断、急停与复位、控制记录；效果报表与待复盘队列；发布记录与「标记健康」 |

前端用 React 19 + Vite + TypeScript + Tailwind CSS 构建，路由用 React Router，产物通过 `go:embed` 嵌入 Go 二进制，部署时只有一个可执行文件。侧栏常驻导航、最近事件、`⌘K` 快速跳转和模型切换。

## 身份：客户端不能自报身份

```go
// 摘自 internal/api/auth.go 的注释（有删减）
// Actor 是服务端判定的请求身份。客户端永远不能自己声明身份：X-Operator 之类的请求头不是身份。
//
// Auth 把每个请求解析成三种身份之一：机器 Token、操作人的个人 Bearer Token、
// 为该 Token 签发的签名会话 Cookie。会话是无状态的，用进程级密钥签名，
// 所以重启会让所有人重新登录；从配置里删掉一个操作人，他的访问立即失效。
```

| 身份 | 怎么证明 | 用途 |
|---|---|---|
| 机器身份 | 共享 Token（`server.auth_token`） | Alertmanager 推告警、CI 登记发布记录、脚本。**永远不带人的权限** |
| 操作人（API） | 个人 Bearer Token（配置在 `web.operators` 里） | 脚本以个人身份调用 |
| 操作人（Web） | 登录时用个人 Token 换取签名会话 Cookie（12 小时） | 浏览器访问 |

会话 Cookie 的属性：`HttpOnly`（脚本读不到）、`SameSite=Strict`（跨站请求不带 Cookie）、HTTPS 部署时加 `Secure`。

**CSRF 防护**：用 Cookie 认证的请求如果要改状态，必须带 `X-Requested-With` 头。浏览器跨站请求带不了自定义头，除非通过 CORS 预检，而本服务从不放行 CORS。`SameSite=Strict` 是第二层防护。

## 权限：三级角色

| 操作 | 最低角色 |
|---|---|
| 查看 incident、事件流、步骤、问题、效果报表 | viewer |
| 批准 / 拒绝审批 | operator |
| 追问、重新诊断、请求补充证据 | operator |
| 记录复盘结论、确认发布已验证 | operator |
| 登记发布记录 | operator，或机器身份（CI 调用） |
| **急停、恢复、重置规则** | **admin** |
| **切换诊断模型** | **admin** |

角色是递进的：admin 包含 operator 的权限，operator 包含 viewer 的权限。

## 先弄懂：SSE（Server-Sent Events）

SSE 是浏览器原生支持的**服务端单向推送**协议：

- 浏览器用 `EventSource` 发起一个普通的 GET 请求；
- 服务端保持连接不关闭，按固定格式持续写入事件：

```text
id: 1024
event: verify.passed
data: {"id":1024,"event_type":"verify.passed","summary":"verification passed: ..."}

```

- 连接断开后，浏览器会**自动重连**，并在请求头 `Last-Event-ID` 里带上最后收到的 id，服务端从那里接着发。

和 WebSocket 相比：SSE 只能服务端到客户端，但基于普通 HTTP，自带重连和续传，实现简单。控制室只需要单向推送，SSE 正合适。

## 事件流的实现：数据库是唯一来源

```go
// 摘自 internal/api/stream.go（有删减）
// EventStore 是 SSE 的持久化接口。它必须实现严格的「id 之后」分页；
// handler 从不把进程内的事件 channel 当作事实来源
type EventStore interface {
	ListIncidentEvents(ctx context.Context, incidentID, afterID uint64, limit int) ([]store.IncidentEvent, error)
}

// 默认：每秒轮询一次；单个连接最长 10 分钟；最多 100 个并发连接
func (h *StreamAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, true); !ok {
		return
	}
	if !h.tryAcquire() { // 连接数上限
		writeError(w, http.StatusServiceUnavailable, "too many event streams")
		return
	}
	defer h.active.Add(-1)
	after, err := streamCursor(r) // 从 Last-Event-ID 或查询参数读取「从哪个 id 之后开始」
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // 让 Nginx 之类的反向代理不要缓冲
	h.writeEvents(w, flusher, ctx, incidentID, &after) // 先把积压的事件发一遍
	poll := time.NewTicker(h.poll)
	deadline := time.NewTimer(h.maxAge)
	for {
		select {
		case <-ctx.Done():  // 浏览器断开
			return
		case <-deadline.C:  // 连接满 10 分钟：主动断开，浏览器会带着 Last-Event-ID 重连
			return
		case <-poll.C:
			h.writeEvents(w, flusher, ctx, incidentID, &after) // 读 id > after 的新事件，写出并推进游标
		}
	}
}
```

**为什么从数据库读，而不是在进程内广播？**

| 进程内 channel 广播 | 数据库按 id 分页轮询 |
|---|---|
| 进程重启，订阅者和缓冲全丢 | 重启后浏览器带 Last-Event-ID 重连，接着读 |
| 可能推送一个最终回滚了的事件 | 只读得到已提交的事件 |
| 断线期间的事件要另外补 | 同一套逻辑：从游标之后读 |
| 需要管理订阅关系 | 无状态，每个连接一个游标 |

代价是每秒一次查询，对控制室的访问量完全可以接受。

**为什么连接最长 10 分钟？** 防止连接无限占用；浏览器会自动重连，用户无感。连接数上限 100 个，防止资源被耗尽。

## 操作都是异步的

作战室的操作按钮都不会在 HTTP 请求里做耗时工作：

```go
// 摘自 cmd/server/main.go（有删减）
// incidentActionService 让 Web 操作保持异步、限定在 incident 范围内。
// 重新诊断只是排队一个 run；补充证据走和追问相同的排队路径，
// 所以 HTTP handler 里从不执行模型或 Docker 工作
func (s *incidentActionService) Rediagnose(ctx context.Context, incidentID uint64, _ api.Actor) (store.AgentRun, error) {
	mode := incident.RouteMode(int(found.Severity), s.severityRoute)
	if mode == incident.ModeSkip {
		mode = incident.ModeLight // 人工重诊不允许 skip
	}
	run, _, err := s.db.RequestRun(ctx, store.RunRequest{IncidentID: incidentID, Mode: mode, Trigger: store.RunTriggerManual /* … */})
	return run, err // 准入被拒绝时，返回原因码（比如冷却中、有处理正在进行）
}

func (s *incidentActionService) RequestEvidence(ctx context.Context, incidentID uint64, actor api.Actor, request string) error {
	_, err := s.questions.Ask(ctx, incidentID, conversation.Actor{ /* … */ }, "请补充采集证据："+request, "web")
	return err
}
```

操作的结果通过事件流实时出现在时间线上，页面不需要刷新。

## 常见追问

- **会话为什么是无状态的？** 签名 Cookie 里包含操作人 ID 和过期时间，服务端验签即可，不需要会话表。代价是进程重启后所有人要重新登录，而且没法单独吊销某个会话；但从配置里删掉操作人，他的所有会话立即失效。
- **机器 Token 为什么不能审批？** 机器身份只用于自动化（推告警、登记发布），它能被脚本、CI 使用，泄露的风险更高。审批这种人的决策，只接受个人身份。
