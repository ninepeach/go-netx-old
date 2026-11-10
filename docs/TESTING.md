# go-netx 测试清单（Test Checklist）

本文档有三个用途：

1. **go-netx 的测试矩阵**：每个检查点有固定 ID，标注当前状态，并对应到具体测试函数。
2. **Review 对照表**：审查代码或 PR 时，按 ID 核对测试是否覆盖，而不是凭感觉判断。
3. **网络库通用问题清单**：第 14 节总结了开发网络库时反复出现的问题类型，以后写其他网络库时可以直接拿来逐项检查。

本文档随代码更新，更新规则见末尾。测试由多方共同补充（包括 `test-matrix` 分支），合并时统一登记到这里。

## 0. 标记说明

**优先级**

- **P0**：核心正确性，必须覆盖。
- **P1**：重要边界、并发和异常路径。
- **P2**：资源上限、可观测性、平台差异和极端情况。

**状态**

| 标记 | 含义 |
|---|---|
| ✅ | 已覆盖 |
| ⚠️ | 部分覆盖，缺什么写在“说明”一栏 |
| ❌ | 尚未覆盖，需要补充测试 |
| 🔧 | 已知代码问题，尚未修复（见第 12 节） |
| ➖ | 不适用：这个 API 有意不提供该行为，“说明”一栏写明原因 |
| ⚙️ | 由命令执行保证，不对应测试函数，命令写在“说明”一栏 |
| ❓ | 设计待定，需要先确定 API 约定 |

## 0.1 文档与测试的对应关系

完整的开发与测试规则（推送前检查、测试写法、平台规则、问题登记）见 [CONTRIBUTING.md](CONTRIBUTING.md)。本节只说明对应关系本身。

**以测试代码为准。** 每个 Test、Benchmark、Fuzz 函数都在注释里声明它覆盖的检查点：

```go
// Checks: UDP-21, UDP-22
func TestReplyValidatesPacket(t *testing.T) {
```

各表的“测试”一栏由工具根据这些声明自动生成，格式为 `包名.函数名`（省略 `Test` 前缀），**不要手工编辑**；“说明”一栏手工维护。

```sh
go run ./internal/tools/testdoc -w   # 根据测试代码重新生成“测试”一栏
go run ./internal/tools/testdoc      # 只检查，不修改
```

`go test ./...` 会通过 DOC-01 自动校验，以下任意一种情况都会导致测试失败：

- 测试函数没有 `// Checks:` 声明；
- 声明了文档中不存在的检查点 ID；
- 标为 ✅ 或 ⚠️ 的检查点没有任何测试；
- 标为 ❌、🔧、➖ 或 ⚙️ 的检查点却已经有测试声明它（说明状态没有更新）；
- 回归清单引用了不存在的检查点；
- 文档的“测试”一栏没有重新生成。

## 1. 通用完成标准

每个模块都要满足以下标准，才能算通过 Review：

1. 每个公开 API 至少有一个正常路径测试。
2. 每个输入参数都有边界测试，包括 0、负数、上限、上限加一。
3. 每个资源申请都有对应的释放路径测试，包括成功、失败和被拒绝三条路径。
4. 每个返回错误的分支都有测试。
5. 每个并发共享的状态都有 race 测试。
6. Close、Shutdown、ctx 取消都有测试，并且覆盖“正在等待时被关闭”的情况。
7. 重要状态机有状态转换测试。
8. 热路径有 Benchmark，并且断言分配次数。
9. 平台相关代码在对应平台上有测试，其他平台至少能交叉编译通过。
10. 所有已发现的回归项都有回归测试（第 11 节）。

## 2. socket：Socket 配置

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| SOCK-01 | P0 | 不设置任何选项时能正常 Listen | ✅ | `socket.ListenConfigWithoutOptions` |  |
| SOCK-02 | P0 | RecvBuffer、SendBuffer 生效 | ✅ | `socket.ListenConfigAppliesBuffersBeforeBind` | RecvBuffer 和 SendBuffer 都检查了（unix） |
| SOCK-03 | P0 | 缓冲区大小在 bind 之前设置（Control 阶段） | ✅ | `socket.ListenConfigAppliesBuffersBeforeBind` | 在 listener 上读取，证明是在 bind 前设置的 |
| SOCK-04 | P1 | accept 出来的 TCP 连接继承 listener 的缓冲区设置 | ✅ | `tcp.AcceptedConnInheritsBuffersDarwin`<br>`tcp.AcceptedConnInheritsBuffers` | 分别在 Linux 和 macOS 上运行 |
| SOCK-05 | P1 | 缓冲区与 ReusePort 组合生效；Linux 上 FastOpen 生效，backlog 正确 | ✅ | `socket.BuffersCombineWithReusePort`<br>`socket.FastOpenAppliesOnLinux` | 仅 Linux |
| SOCK-06 | P1 | 同一份带 FastOpen 的配置在所有平台都能 Listen、Dial 并正常通信：平台不支持时不带 FastOpen 运行（D-18） | ✅ | `socket.FastOpenConfigurationRunsEverywhere` | 所有平台运行 |
| SOCK-07 | P1 | 没有“忽略不支持”的开关（`UnsupportedPolicy`、`UnsupportedIgnore` 已删除）；性能选项按 D-18 处理，影响行为的选项不支持时报错 | ➖ |  | v1.1.0 删除开关，见 D-13、D-18 |
| SOCK-08 | P1 | 内核表示不支持 FastOpen 的错误（ENOPROTOOPT、EOPNOTSUPP、ENOTSUP）时照常创建 socket；其他系统错误原样返回 | ✅ | `socket.FastOpenRunsWithoutWhenKernelLacksIt` | unix 平台 |
| SOCK-09 | P1 | `socket.Dialer` 返回副本，不修改调用方传入的 `net.Dialer` | ✅ | `socket.DialerCopiesBaseAndAppliesBuffers` |  |
| SOCK-10 | P1 | 缓冲区为 0 或负数时不设置 | ✅ | `socket.ZeroAndNegativeBuffersAreNotApplied` | 仅 Linux |
| SOCK-11 | P2 | `SetBuffers` 作用于已存在的连接 | ✅ | `socket.SetBuffersOnExistingConnection` |  |
| SOCK-12 | P2 | 各平台都能编译 | ⚙️ |  | 交叉编译，见 NF-17 |
| SOCK-13 | P1 | keepalive 的空闲时间、探测间隔、探测次数作用于 listener，accept 出来的连接继承 | ✅ | `socket.KeepAliveAppliesToAcceptedConns` | 仅 Linux |
| SOCK-14 | P1 | `KeepAlive.Disable` 关闭 keepalive | ✅ | `socket.KeepAliveDisable` | 仅 Linux |
| SOCK-15 | P1 | Dialer 拨出的连接同样应用 keepalive 设置 | ✅ | `socket.KeepAliveAppliesToDialer` | 仅 Linux |
| SOCK-16 | P2 | 不设置 keepalive 时保持 Go 的默认行为（开启，15 秒） | ✅ | `socket.KeepAliveZeroValueKeepsGoDefault` | 仅 Linux |
| SOCK-17 | P1 | tcp、udp 以外的网络类型（如 unix）设置了任何选项时返回 `UnsupportedOptionError`，不再静默忽略 | ✅ | `socket.OptionsOnOtherNetworksAreRefused` | unix socket 上验证（unix 平台） |
| SOCK-18 | P1 | 在 UDP 上设置 FastOpen 或 KeepAlive（只适用于 TCP）时返回 `UnsupportedOptionError`，Listen 和 Dialer 都一样；始终显式报错 | ✅ | `socket.TCPOnlyOptionsOnUDPAreRefused` |  |
| SOCK-19 | P1 | 不设置任何选项时，任何网络类型都能正常工作 | ✅ | `socket.ZeroOptionsWorkOnAnyNetwork` |  |
| SOCK-20 | P1 | macOS：ReusePort 设置 `SO_REUSEPORT`，同一端口可以绑定第二个 socket（不分流） | ⚠️ | `socket.ReusePortOnBSD` | 测试已写好，只在 macOS 上运行；当前开发环境只做了编译验证 |
| SOCK-21 | P2 | FreeBSD：ReusePort 使用分流的 `SO_REUSEPORT_LB` | ⚠️ | `socket.ReusePortOnBSD` | 测试已写好，只在 FreeBSD 上运行；当前开发环境只做了编译验证 |
| SOCK-22 | P0 | RecvBuffer/SendBuffer 设置后读回实际值，小于请求值时返回 `BufferSizeError`（Option、Requested、Actual 正确，Linux 读数减半），不是 `UnsupportedOptionError` | ✅ | `socket.EnsureBufferReportsShortSize` | 用模拟内核测试同一段逻辑，所有平台运行 |
| SOCK-23 | P1 | 不足时（仅 Linux）用 FORCE 选项重试一次，FORCE 自身的错误被忽略，以重试后读回的值为准；足够时不重试；setsockopt、getsockopt 的错误原样返回 | ✅ | `socket.EnsureBufferTriesForceOnce` | 所有平台运行 |
| SOCK-24 | P0 | Linux 真实内核：请求值不超过 rmem_max/wmem_max 时成功；超过时，有 CAP_NET_ADMIN 则拿到完整大小，没有则返回 `BufferSizeError`，Actual 等于上限 | ✅ | `socket.BufferCapOnLinux` | 运行时检测权限，分别断言两种结果；已用 root 和 `setpriv --bounding-set -net_admin` 各跑一次 |
| SOCK-25 | P1 | `SetBuffers` 作用于已有连接时同样校验 | ✅ | `socket.BufferCapOnLinux` | 仅 Linux |
| SOCK-26 | P1 | macOS：内核开启服务端 Fast Open（`net.inet.tcp.fastopen` 含 2）时，`socket.Listen` 在 listen 之后设置 `TCP_FASTOPEN` 并生效；只用 `ListenConfig` 时不带 Fast Open 监听、不报错；内核关闭时监听照常工作 | ⚠️ | `socket.FastOpenOnDarwin` | 只在 macOS 上运行，由 CI 实际运行，CI 首次通过后改为 ✅ |
| SOCK-27 | P1 | `socket.Supported()` 如实报告当前平台实现了哪些性能选项（Linux、macOS、FreeBSD、其他） | ✅ | `socket.SupportedMatchesPlatform` | 所有平台运行 |
| SOCK-28 | P0 | `socket.Dialer` 设置了 FastOpen 时拨号报 `UnsupportedOptionError`：拨出方向的 FastOpen 会让已关闭的对端看起来连接成功（D-19） | ✅ | `socket.DialerRefusesFastOpen` |  |
| SOCK-29 | P0 | `socket.Dialer` 保留调用方的 `Control` 和 `ControlContext`，先执行调用方的，再应用 go-netx 的选项；base 设置了 `ControlContext` 时选项照样生效 | ✅ | `socket.DialerKeepsCallerControl` |  |
| SOCK-30 | P1 | `SetBuffers` 收到 nil 或 typed nil 连接时返回错误，不 panic，也不假装成功 | ✅ | `socket.SetBuffersRejectsNil` |  |

## 3. tcp：TCP 传输

### 3.1 Listen 与 Accept

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| TCP-01 | P0 | Listen 成功并能接受连接 | ✅ | `tcp.EchoAndStats` |  |
| TCP-02 | P0 | 端口已被占用时返回错误 | ✅ | `tcp.ListenAddressInUse` |  |
| TCP-03 | P0 | 多个客户端连续连接都能被接受 | ✅ | `integration.TCPLineEcho` |  |
| TCP-04 | P0 | 关闭后 Serve 返回，再次 Serve 返回 `ErrClosed` | ✅ | `tcp.ServeContextCancelAborts` |  |
| TCP-05 | P1 | 临时 accept 错误触发退避重试，不会终止 | ✅ | `tcp.TemporaryAcceptErrorsBackOffAndStopPromptly` |  |
| TCP-06 | P1 | 永久 accept 错误会 abort 所有连接并返回错误 | ✅ | `tcp.PermanentAcceptErrorAbortsHandlers` |  |
| TCP-07 | P1 | 退避期间 Close/Shutdown 能立即唤醒 | ✅ | `backoff.WaitStopsEarly`<br>`tcp.TemporaryAcceptErrorsBackOffAndStopPromptly` |  |
| TCP-08 | P1 | 退避时间有上限（1 秒） | ✅ | `backoff.NextDoublesAndCaps` |  |
| TCP-09 | P1 | 并发多次调用 Close/Shutdown 不会 panic | ✅ | `tcp.ConcurrentCloseAndShutdown` |  |
| TCP-10 | P1 | `Serve` 被调用两次时返回错误 | ✅ | `tcp.ServeTwice` |  |
| TCP-11 | P1 | `NewServer` 传入 nil listener 或 nil handler 时返回错误 | ✅ | `tcp.NewServerRejectsNil` |  |

### 3.2 连接生命周期

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| TCP-12 | P0 | 每个连接调用一次 handler，并分配唯一 ID | ✅ | `tcp.ConnIDsUnique` |  |
| TCP-13 | P0 | handler 返回后连接被关闭，Stats 中 Active 归零 | ✅ | `tcp.EchoAndStats` |  |
| TCP-14 | P0 | Admit 拒绝时关闭连接并计数 | ✅ | `tcp.AdmitRejects` |  |
| TCP-15 | P1 | Admit 的许可在所有路径上成对释放 | ✅ | `tcp.AdmitRejects`<br>`tcp.ConnAcceptedDuringStopIsReleased` |  |
| TCP-16 | P1 | Drain：停止 accept，现有 handler 可以继续运行，ctx 不会被取消 | ✅ | `tcp.ShutdownDrainsThenReturns` |  |
| TCP-17 | P1 | Drain 超时后 abort：ctx 被取消，连接被关闭 | ✅ | `tcp.ShutdownAbortsOnTimeout` |  |
| TCP-18 | P1 | `Draining()` 在 StopAccept 时关闭 | ✅ | `tcp.ShutdownDrainsThenReturns` |  |
| TCP-19 | P1 | 关闭期间 accept 进来的连接不会漏登记，也不会泄漏 | ✅ | `tcp.ConnAcceptedDuringStopIsReleased` |  |
| TCP-20 | P1 | Serve 的 ctx 被取消等价于 abort | ✅ | `tcp.ServeContextCancelAborts` |  |
| TCP-21 | P1 | handler 能读到 Serve ctx 中的值 | ✅ | `tcp.HandlerSeesServeContextValues` |  |
| TCP-22 | P2 | 大量连接建立和断开后，goroutine 和 FD 数量不持续增长 | ✅ | `tcp.StressTCPChurn` | 压测，`make stress` |
| TCP-23 | P0 | `MaxConnsPerIP`：同一 IP 超出上限的连接被拒绝并计入 `RejectedPerIP`，其他 IP 不受影响 | ✅ | `tcp.MaxConnsPerIP` | “其他 IP 不受影响”只在 Linux 上验证（需要 127.0.0.2） |
| TCP-24 | P1 | 按 IP 的额度在连接关闭时释放，在 Admit 拒绝时也会退还 | ✅ | `tcp.AdmitRejectionReturnsPerIPSlot`<br>`tcp.PerIPSlotReleasedOnClose` | 压测中两种拒绝同时发生，结束后额度全部归还 |
| TCP-25 | P1 | IPv6 按前缀归组（默认 /64），IPv4 按单个地址，IPv4 映射地址按 IPv4 处理 | ✅ | `tcp.IPKeyGrouping` |  |
| TCP-26 | P2 | 远端地址不是 TCP 地址的连接（自定义 listener）不做按 IP 限制 | ✅ | `tcp.PerIPSkipsNonTCPAddresses` |  |
| TCP-27 | P1 | `MaxConnsPerIP` 为负数、`IPv6PrefixBits` 超出 0～128 时 Listen/NewServer 返回错误 | ✅ | `tcp.PerIPOptionValidation` |  |
| TCP-28 | P1 | `tcp6` 监听 `[::1]`：收发正常，两端地址都是 IPv6；Socket 选项（收发缓冲区、KeepAlive）在 IPv6 监听上生效；`MaxConnsPerIP` 对 `::1` 按 /64 计数并拒绝超额连接 | ⚠️ | `tcp.IPv6Loopback` | 开发环境没有 IPv6 会跳过；由 CI（`NETX_REQUIRE_IPV6=1`，缺 IPv6 直接失败）实际运行，CI 首次通过后改为 ✅ |
| TCP-29 | P1 | 双栈监听 `[::]` 同时接受 IPv4 与 IPv6 客户端；IPv4 映射地址按 IPv4 计入 `MaxConnsPerIP`，不占 IPv6 客户端的额度 | ⚠️ | `tcp.DualStackAcceptsIPv4` | 同上 |

### 3.3 Dialer

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| DIAL-01 | P0 | 正常连接 | ✅ | `tcp.Dialer` |  |
| DIAL-02 | P0 | 连接失败时返回错误，并触发 `OnDial` | ✅ | `tcp.DialEventFailure` |  |
| DIAL-03 | P0 | ctx 取消后拨号及时结束 | ✅ | `tcp.DialContextCancelled` |  |
| DIAL-04 | P0 | 超时设置生效 | ✅ | `tcp.DialTimeout` |  |
| DIAL-05 | P1 | 成功时 `DialEvent` 带有本地和远端地址 | ✅ | `tcp.DialEventSuccess` |  |
| DIAL-06 | P1 | socket 选项作用于拨出的连接 | ✅ | `tcp.DialerAppliesSocketOptions` | 仅 Linux |
| DIAL-07 | P1 | 回调出错时的处理 | ➖ |  | `OnDial` 没有返回值，只做观测 |
| DIAL-08 | P1 | 并发拨号没有共享状态竞争 | ✅ | `tcp.ConcurrentDials` |  |

## 4. udp：UDP 传输

### 4.1 Listen 与接收

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| UDP-01 | P0 | Listen 后能正常收发 | ✅ | `udp.Echo` |  |
| UDP-02 | P0 | 开启 ReusePort 且 Readers 大于 1 时，包会分散到不同读循环 | ✅ | `udp.ReusePortReaders` | 仅 Linux |
| UDP-03 | P0 | 收到的数据和来源地址正确，IPv4 映射地址已还原；同一双栈 socket 上的 IPv6 发送方报告为 IPv6，回复可达 | ✅ | `udp.FromAddressIsSenderAndUnmapped` | 无 IPv6 回环时跳过（否则 Go 会把 `[::]` 静默开成 IPv4 socket，测试形同虚设）；CI 设置 `NETX_REQUIRE_IPV6=1` |
| UDP-04 | P0 | 零长度数据报能正常交给 handler | ✅ | `udp.ZeroLengthDatagram` |  |
| UDP-05 | P0 | 恰好 `MaxPacket` 字节的包能收到 | ✅ | `udp.DefaultMaxDatagram` |  |
| UDP-06 | P1 | 超过 `MaxPacket` 的包被丢弃并计入 Truncated（Linux 靠 MSG_TRUNC 标志，Windows 靠 WSAEMSGSIZE 错误） | ✅ | `udp.DefaultMaxDatagram`<br>`udp.TruncatedDropped` |  |
| UDP-07 | P1 | 多个读循环并发运行没有数据竞争 | ✅ | `udp.ConcurrentWritesAndClose`<br>`udp.ReusePortReaders` | 在 -race 下运行 |
| UDP-08 | P1 | 读错误分类正确：暂时性、截断、致命 | ✅ | `sys.ClassifyRead` |  |
| UDP-09 | P1 | 暂时性错误按上限 1 秒退避重试 | ✅ | `backoff.NextDoublesAndCaps`<br>`udp.FatalReadErrorStopsServe` |  |
| UDP-10 | P1 | 退避期间调用 Close 能立即唤醒 | ✅ | `backoff.WaitStopsEarly`<br>`udp.CloseInterruptsBackoff` |  |
| UDP-11 | P1 | 致命错误关闭整个 Conn，所有读循环退出，Serve 返回该错误 | ✅ | `udp.FatalReadErrorStopsServe` |  |
| UDP-12 | P1 | 关闭、ctx 取消后所有读循环退出，再次 Serve 返回 `ErrClosed` | ✅ | `udp.ServeAfterCloseAndContextCancel` |  |
| UDP-13 | P2 | `Readers` 为 0 时取默认值、为负数时拒绝；不设上限（D-01） | ✅ | `udp.ListenOptionBoundaries` | 上限按 D-01 交给调用方 |
| UDP-14 | P2 | `MaxPacket` 为 0 时取默认值、为负数时拒绝、超过 `MaxDatagram` 时拒绝 | ✅ | `udp.ListenOptionBoundaries`<br>`udp.MaxPacketBound` | 0 和负数、上限都已覆盖 |
| UDP-15 | P2 | 读循环的 handler 路径零分配 | ✅ | `udp.HandlerDoesNotAllocate` |  |
| UDP-32 | P0 | 排队的一批数据报（来自两个发送方，超过一个 batch）全部交给 handler，每个发送方内部顺序、内容、来源地址都正确，统计一致 | ✅ | `udp.BurstFromTwoSendersDeliveredInOrder` | Linux 上走 recvmmsg（默认 Batch 8），其他平台走单包读取 |
| UDP-33 | P1 | `Batch` 为 0 时取默认值、为负数时拒绝、超过 1024 时拒绝；上下限在所有平台相同 | ✅ | `udp.BatchOptionBoundaries` | 所有平台运行 |
| UDP-34 | P1 | `Batch: 1` 走单包读取路径，同样零分配 | ✅ | `udp.HandlerDoesNotAllocate` |  |
| UDP-35 | P1 | 一批中间夹着超过 `MaxPacket` 的数据报时，只丢弃并计数这一个，前后的照常交付 | ✅ | `udp.TruncatedInsideBurst` |  |
| UDP-36 | P1 | 批量读取时内核丢包计数仍然生效 | ✅ | `udp.KernelDropsCountedWithBatch` | 仅 Linux |
| UDP-37 | P1 | `Conn.Batch()` 返回实际生效的批量：Linux 上为设置值或默认 8；没有 recvmmsg 的平台上任何设置都为 1，且照常收发 | ✅ | `udp.BatchInEffect` | 按平台分别断言 |
| UDP-38 | P1 | 多个 reader 共用一个 socket（`Readers` > 1 且未开 ReusePort）时，`Batch` 默认取 1，显式设置的值照用 | ✅ | `udp.BatchDefaultWhenReadersShareASocket` |  |

### 4.2 发送与 Reply

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| UDP-16 | P0 | `WriteTo` 发送内容正确 | ✅ | `udp.ReplyValidatesPacket` | 经由 Reply 的回退路径 |
| UDP-17 | P0 | `Reply` 回包内容正确 | ✅ | `udp.Echo` |  |
| UDP-18 | P0 | 超过 `MaxDatagram` 时返回 `ErrTooLarge`；较小的 `MaxPacket` 不限制发送 | ✅ | `udp.WriteTooLarge` |  |
| UDP-19 | P1 | IPv4 下 65000 字节能发出（低于 IPv4 理论上限 65507）；macOS 上同样能发出（默认发送缓冲区 9216 字节，Listen 会调高） | ✅ | `udp.DefaultMaxDatagram`<br>`udp.IPv4MaxDatagramRoundTrip` | macOS 部分由 CI 运行 |
| UDP-20 | P1 | IPv6 下的发送边界 | ⚠️ | `udp.IPv6MaxDatagramRoundTrip` | 开发环境没有 IPv6 会跳过；由 CI（`NETX_REQUIRE_IPV6=1`）实际运行，CI 首次通过后改为 ✅ |
| UDP-21 | P1 | `Reply(nil)` 返回 `ErrNilPacket` | ✅ | `udp.ReplyValidatesPacket` |  |
| UDP-22 | P1 | Reader 为负数或越界时回退到按端口选 socket，不 panic | ✅ | `udp.ReplyValidatesPacket` |  |
| UDP-23 | P1 | 并发调用 WriteTo、Reply，同时 Close | ✅ | `udp.ConcurrentWritesAndClose` |  |
| UDP-24 | P1 | 关闭后发送返回 `ErrClosed` | ✅ | `udp.ConcurrentWritesAndClose`<br>`udp.WriteAfterCloseIsErrClosedAndCounted` |  |
| UDP-25 | P1 | 并发多次调用 Shutdown | ✅ | `udp.ConcurrentShutdown` |  |

### 4.3 内核丢包计数

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| UDP-26 | P1 | Linux 上成功开启 SO_RXQ_OVFL | ✅ | `udp.KernelDropsEnabled` |  |
| UDP-27 | P1 | 不支持时 `KernelDropsEnabled` 为 false，不会冒充“零丢包” | ✅ | `udp.KernelDropsEnabled` | 在非 Linux 平台验证为 false |
| UDP-28 | P1 | 32 位计数回绕后，累计值仍然正确 | ✅ | `udp.DropCounterWraps` |  |
| UDP-29 | P1 | 共享同一 socket 的多个读循环上报的旧值不会被重复累加 | ✅ | `udp.DropCounterWrapAndStaleReports`<br>`udp.DropCounterWraps` |  |
| UDP-30 | P1 | cmsg 解析：能找到目标项，忽略无关项，拒绝截断数据和虚报长度 | ✅ | `sys.DropCountParse` |  |
| UDP-31 | P2 | 真实丢包场景下计数会增加 | ✅ | `udp.StressKernelDropsAccounting` | 压测，仅 Linux；断言“收到数 + 内核丢包数 = 发送数”。内核在下一个入队的数据报上才上报丢包，见 `Stats.KernelDrops` 的注释 |

## 5. flow：Flow 表

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| FLOW-01 | P0 | ID 唯一、严格递增，0 不会被分配 | ✅ | `flow.IDsIncrease` |  |
| FLOW-02 | P0 | `GetOrOpen` 不存在时创建，存在时复用 | ✅ | `flow.RemoveChecksEpoch` |  |
| FLOW-03 | P0 | 状态机：Open → Closing → Closed，`BeginClose` 只在第一次调用时成功 | ✅ | `flow.CloseOnce` |  |
| FLOW-04 | P0 | 并发 Close 时只有一个调用者拿到 true | ✅ | `flow.CloseOnce` |  |
| FLOW-05 | P1 | `Remove` 校验 ID（epoch），旧 ID 删不掉复用了同一 key 的新 flow | ✅ | `flow.RemoveChecksEpoch` |  |
| FLOW-06 | P1 | 无论通过 Remove、Sweep 还是 Close 离开表，`OnClose` 都恰好调用一次 | ✅ | `flow.ConcurrentRemovalClosesFlowOnce` | Remove、Sweep、Close 三者并发 |
| FLOW-07 | P1 | 同一个 key 并发 `GetOrOpen` 只创建一个 flow | ✅ | `flow.GetOrOpenConcurrentSingleCreation` |  |
| FLOW-08 | P1 | 闲置的 flow 会被清理 | ✅ | `flow.ActiveFlowSurvivesSweep`<br>`flow.SweepIdle` |  |
| FLOW-09 | P1 | 持续被访问的 flow 在 Run 并发清扫时不会被清理 | ✅ | `flow.ActiveFlowSurvivesSweep` |  |
| FLOW-10 | P1 | 不运行 Run、只手动 Sweep 时也会推进时钟 | ✅ | `flow.ManualSweepKeepsTouchedFlow` |  |
| FLOW-11 | P1 | 传入更早的时间时时钟不会倒退 | ✅ | `flow.SweepClockNeverMovesBackward` |  |
| FLOW-12 | P1 | 多个 Sweep 并发执行时不会重复调用 `OnClose` | ✅ | `flow.ConcurrentRemovalClosesFlowOnce`<br>`flow.ConcurrentSweepsCloseOnce` |  |
| FLOW-13 | P1 | Run 退出时关闭所有 flow | ✅ | `flow.RunClosesAllOnExit` |  |
| FLOW-14 | P2 | 大量 flow 下 Sweep 的耗时 | ✅ | `flow.BenchmarkSweepBySize` | 实测（2 vCPU）：1 万 flow 约 0.1ms，10 万约 1.4ms，两者多次运行稳定；100 万时表超出 CPU 缓存，结果随机器变化很大：2026-11-06 在一台 VM 上多次运行为 19～50ms，2026-11-05 在另一台 VM 上为 105ms。单个 flow 的成本随规模上升，原因是缓存未命中 |
| FLOW-15 | P2 | 不同 key 哈希冲突时不会被合并 | ➖ |  | 用的是 Go map，key 按值比较，哈希只用来选分片 |
| FLOW-16 | P2 | 查找零分配 | ✅ | `flow.BenchmarkGetAddrPort`<br>`flow.BenchmarkGetAndTouch`<br>`flow.BenchmarkGetOrOpenExisting` |  |
| FLOW-17 | P1 | 调用方先 `Flow.Close()`，再经 Remove、Sweep 或 Table.Close 离开表，`OnClose` 仍恰好调用一次 | ✅ | `flow.OnCloseRunsForFlowsClosedByTheCaller` |  |
| FLOW-18 | P1 | `GetOrOpen` 的 newValue panic 后，panic 照常向上传播，分片锁被释放，表照常可用，且没有插入 flow | ✅ | `flow.GetOrOpenPanicReleasesShard` |  |
| FLOW-19 | P1 | `Range` 的 fn panic 后，panic 照常向上传播，分片锁被释放，表照常可用 | ✅ | `flow.RangePanicReleasesShard` |  |
| FLOW-20 | P0 | `Get` 是纯查找，不更新 LastSeen | ✅ | `flow.GetDoesNotTouch` |  |
| FLOW-21 | P0 | `GetOrOpen` 找到已有 flow 时不更新 LastSeen，也不调用 newValue | ✅ | `flow.GetOrOpenExistingDoesNotTouch` |  |
| FLOW-22 | P0 | 新 flow 创建时 LastSeen 已初始化：表内为表时钟，`New` 为当前时间 | ✅ | `flow.NewFlowLastSeenInitialised` |  |
| FLOW-23 | P0 | `Touch` 把 LastSeen 更新为表时钟（`New` 创建的用当前时间），不改变 State，不会倒退；`GetAndTouch` 同样生效 | ✅ | `flow.TouchUpdatesLastSeen` |  |
| FLOW-24 | P1 | 并发 `Touch` 安全，结果为最新时钟 | ✅ | `flow.ConcurrentTouch` |  |
| FLOW-25 | P0 | Closing、Closed 的 flow 不能被 `Touch` 重新激活（LastSeen 不变，State 不变）；仍按旧 LastSeen 被 Sweep 清理 | ✅ | `flow.TouchDoesNotReviveClosingOrClosed` |  |
| FLOW-26 | P0 | 只 `Get`、`GetOrOpen`、`Range` 而不 `Touch` 时，flow 照常按空闲超时被清理 | ✅ | `flow.GetWithoutTouchStillExpires` |  |
| FLOW-27 | P0 | `Get` 后 `Touch`，在 IdleTimeout 内不会被清理，超过后被清理 | ✅ | `flow.TouchKeepsFlowUntilIdle` |  |
| FLOW-28 | P0 | `Touch` 与 `Sweep` 并发：要么 Touch 先生效、flow 保留且 LastSeen 为新值，要么 Sweep 先决定、flow 以旧 LastSeen 关闭并离开表、OnClose 一次；之后 Touch 无效 | ✅ | `flow.TouchRacingSweep` |  |
| FLOW-29 | P1 | `Remove` 之后 `Touch` 无效：LastSeen 不变、状态为 Closed、不会回到表中 | ✅ | `flow.TouchAfterRemoveHasNoEffect` |  |

## 6. limit：资源控制

### 6.1 Concurrency

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| RES-01 | P0 | 未达上限时允许，达到上限时拒绝，释放后恢复 | ✅ | `limit.Concurrency` |  |
| RES-02 | P0 | 并发情况下持有者数量不超过上限 | ✅ | `limit.ConcurrencyParallel` |  |
| RES-03 | P0 | `InUse` 在任何时刻都不超过 `Max` | ✅ | `limit.ConcurrencyNeverExceedsMax` |  |
| RES-04 | P1 | Release 次数多于 Acquire 时不会多出额度 | ✅ | `limit.ConcurrencyOverReleasePanics` |  |
| RES-05 | P1 | `max <= 0` 时 panic | ✅ | `limit.NewConcurrencyRejectsNonPositive` |  |
| RES-06 | P1 | 等待、ctx 取消、超时 | ➖ |  | `TryAcquire` 从不阻塞，这是设计上的选择 |

### 6.2 Rate 和 All

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| RES-07 | P0 | 令牌桶的突发上限和补充速率正确 | ✅ | `limit.Rate` |  |
| RES-08 | P1 | `All` 部分成功时会释放已经拿到的许可 | ✅ | `limit.AllReleasesOnPartialFailure` |  |
| RES-09 | P1 | 参数非法时 panic | ✅ | `limit.NewRateRejectsNonPositive` |  |

### 6.3 PerKey

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| RES-16 | P0 | 每个 key 独立计数，达到上限时拒绝，释放后恢复 | ✅ | `limit.PerKeyLimitsEachKey` |  |
| RES-17 | P1 | 并发下任何 key 都不超过上限；计数归零后 key 被删除，内存不随历史 key 增长 | ✅ | `limit.PerKeyConcurrentAndForgetsKeys` |  |
| RES-18 | P1 | 对没有持有额度的 key 调用 Release 会 panic；`max <= 0` 时构造函数 panic | ✅ | `limit.PerKeyMisuse` |  |

### 6.4 Budget

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| RES-10 | P0 | 预留、拒绝、释放的数值正确 | ✅ | `limit.Budget` |  |
| RES-11 | P1 | 负数预留被拒绝 | ✅ | `limit.BudgetRefusesNegative` |  |
| RES-12 | P1 | 释放负数或超过已预留的量时 panic | ✅ | `limit.BudgetRefusesNegative` |  |
| RES-13 | P1 | 已有占用时预留 `MaxInt64` 不会溢出，恰好填满时成功 | ✅ | `limit.BudgetOverflow` |  |
| RES-14 | P1 | 并发预留时 `Used` 不超过 `Max` | ✅ | `limit.BudgetParallelNeverExceeds` |  |
| RES-15 | P1 | `max <= 0` 时 panic | ✅ | `limit.NewBudgetRejectsNonPositive` |  |

## 7. queue：有界队列

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| QUEUE-01 | P0 | 先进先出，顺序正确 | ✅ | `queue.FIFOOrder` |  |
| QUEUE-02 | P0 | 容量向上取整为 2 的幂 | ✅ | `queue.CapRoundsUp` |  |
| QUEUE-03 | P0 | Reject：满时拒绝，所有权仍归调用方 | ✅ | `queue.RejectKeepsOwnership` |  |
| QUEUE-04 | P0 | DropOldest：挤掉最旧的元素并交给 `OnDrop` | ✅ | `queue.DropOldestCallsOnDrop` |  |
| QUEUE-05 | P1 | 容量为 0 或负数时 panic | ✅ | `queue.NewRejectsNonPositiveCap` |  |
| QUEUE-06 | P1 | 关闭后拒绝新元素，已有元素仍能取出，之后返回 `ErrClosed` | ✅ | `queue.CloseDrainsThenErrClosed` |  |
| QUEUE-07 | P1 | 重复 Close 是安全的 | ✅ | `queue.CloseRacingPushPreservesBound`<br>`queue.RepeatedClose` |  |
| QUEUE-08 | P1 | Close 唤醒所有正在等待的消费者 | ✅ | `queue.CloseWakesWaiters` |  |
| QUEUE-09 | P1 | Close 唤醒正在等待的生产者（`PushWait`） | ✅ | `queue.CloseWakesBlockedProducer` |  |
| QUEUE-10 | P1 | `PushWait` 在有空位时被唤醒 | ✅ | `queue.PushWaitUnblocksOnPop` |  |
| QUEUE-11 | P1 | `PushWait` 和 `PopWait` 能被 ctx 取消 | ✅ | `queue.PopWaitContext`<br>`queue.PushWaitContext` |  |
| QUEUE-12 | P1 | Drain 把剩余元素逐个交给 `OnDrop` | ✅ | `queue.DrainReleases` |  |
| QUEUE-13 | P1 | 多生产者、单消费者：不丢失、不重复 | ✅ | `queue.ConcurrentNoLoss` |  |
| QUEUE-14 | P1 | 多消费者：不丢失、不重复，也不会丢失唤醒信号 | ✅ | `queue.MultiConsumerNoLossNoDuplicate` |  |
| QUEUE-15 | P2 | Stats 的各项计数与实际操作一致 | ✅ | `queue.RejectKeepsOwnership`<br>`queue.StatsCountEveryOutcome` |  |
| QUEUE-16 | P2 | Push 和 Pop 零分配 | ✅ | `queue.BenchmarkPushPop` |  |
| QUEUE-17 | P1 | Close 与并发 Push 同时发生时，长度不超过容量，关闭后拒绝新元素 | ✅ | `queue.CloseRacingPushPreservesBound` |  |

## 8. buf：缓冲区

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| BUF-01 | P0 | 按大小选到正确的档位（2 的幂，64B…64KB）；超大请求不进池 | ✅ | `buf.GetClasses` |  |
| BUF-02 | P0 | `len(B)` 等于请求的大小 | ✅ | `buf.GetClasses` |  |
| BUF-03 | P0 | `Copy` 不与源数据共享内存 | ✅ | `buf.Copy` |  |
| BUF-04 | P0 | 预算按容量扣除，释放后归还，超出时 Get 返回 nil | ✅ | `buf.BudgetChargesCapacity`<br>`buf.BudgetRejectsLargeBufferWithoutPartialCharge`<br>`buf.ReleaseClearsViewAndRefundsUnpooledCapacity` |  |
| BUF-05 | P1 | 在 `netxdebug` 下重复 Release 会 panic | ✅ | `buf.DoubleReleasePanicsInDebug` |  |
| BUF-06 | P1 | 在 `netxdebug` 下 `Outstanding` 能反映泄漏 | ✅ | `integration.UDPGatewayComposition` |  |
| BUF-07 | P1 | 释放后继续使用的行为 | ➖ |  | 约定为调用方的 bug，正式构建中无法检测 |
| BUF-08 | P1 | 并发 Get 不会返回同一块正在使用的内存 | ✅ | `buf.ConcurrentGetNeverShares` |  |
| BUF-09 | P1 | 负数大小时 panic | ✅ | `buf.GetNegativeSizePanics` |  |
| BUF-10 | P1 | 扩容、Reset | ➖ |  | 没有这两个 API，Buf 的大小固定 |
| BUF-11 | P2 | GC 回收池中对象后，正确性不受影响 | ✅ | `buf.PoolCorrectAcrossGC` |  |
| BUF-12 | P2 | 热路径零分配 | ✅ | `buf.BenchmarkGetReleaseBySize`<br>`buf.BenchmarkGetRelease` |  |
| BUF-13 | P1 | Release 后 `B` 被置为 nil，避免继续使用旧的切片 | ✅ | `buf.ReleaseClearsViewAndRefundsUnpooledCapacity` |  |
| BUF-14 | P1 | 预算不足时不会先扣掉一部分 | ✅ | `buf.BudgetRejectsLargeBufferWithoutPartialCharge` |  |
| BUF-15 | P0 | 档位都是 2 的幂，共 11 档（64B…64KB）；容量不小于请求、从 64B 起小于请求的 2 倍；`Capacity(n)` 与 `Get(n)` 的容量和预算扣除一致 | ✅ | `buf.PowerOfTwoClasses` |  |
| BUF-16 | P1 | 对 nil 调用 `Release` 什么都不做（预算不足时 Get 返回的 nil 可以直接 Release） | ✅ | `buf.ReleaseNilIsNoop` |  |
| BUF-17 | P1 | `netxdebug` 下 Release 把整块缓冲填成 `Poison`，释放后的读取和下一个持有者看不到旧数据 | ✅ | `buf.ReleasePoisonsInDebug` | 仅 netxdebug 构建 |

## 8.5 stream：流式读写

### 8.5.1 Reader

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| STREAM-01 | P0 | 数据分多次到达时，`Peek(n)` 能累积出完整的 n 字节（帧跨多次读取） | ✅ | `stream.PeekAccumulatesAcrossReads` |  |
| STREAM-02 | P0 | `Discard` 消费数据；`Read` 符合 io.Reader 语义 | ✅ | `stream.DiscardAndRead` |  |
| STREAM-03 | P1 | 空闲时只持有 IdleSize 的小缓冲区；需要时升级到更大档位；数据处理完且流量小时退回，大缓冲区还给池（预算归零） | ✅ | `stream.GrowsWhenNeededAndShrinksWhenQuiet`<br>`stream.StressBurstThenIdleReturnsBuffers` | 单元测试覆盖升级和退回；压测中 500 个连接经历大帧后全部退回，预算归零 |
| STREAM-04 | P1 | 需要的数据超过 Max 时返回 `ErrTooLarge`，不会无限增长 | ✅ | `stream.MaxBoundsBuffering` |  |
| STREAM-05 | P1 | 池的预算不足时返回 `ErrNoBuffer`，已缓冲的数据不丢失；预算恢复后继续工作 | ✅ | `stream.BudgetExhaustedKeepsData` |  |
| STREAM-06 | P1 | 底层读错误（包括 EOF）正确传递，已缓冲的数据先交给调用方 | ✅ | `stream.SourceErrorsAfterData` |  |
| STREAM-07 | P1 | `Release` 归还所有缓冲区，之后的调用返回错误；选项非法时构造函数 panic | ✅ | `stream.ReleaseAndOptions` |  |
| STREAM-08 | P2 | 大量空闲连接下，每个连接的缓冲区开销（压测中实测，并与固定 4KB 缓冲区对比） | ✅ | `stream.StressIdleReaderMemory` | 实测（4000 个空闲连接）：每个连接比固定 4KB 的 bufio.Reader 少约 3.6KB，比不用缓冲区多约 430 字节 |
| STREAM-16 | P1 | 数据源连续多次返回 `(0, nil)` 时，`Peek`、`Fill`、`Read` 返回 `io.ErrNoProgress`，不空转 | ✅ | `stream.EmptyReadsDoNotSpin` |  |
| STREAM-17 | P0 | 负数参数：`Peek(n<0)` 返回 `ErrNegativeCount`，`Discard(n<=0)` 返回 0；两者都不改变内部状态，之后的调用照常工作 | ✅ | `stream.NegativeCounts` | Fuzz 覆盖负数参数 |

### 8.5.2 Writer

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| STREAM-10 | P0 | `Flush` 按顺序写出所有 `Add` 的缓冲区，写完全部释放 | ✅ | `stream.FlushWritesInOrderAndReleases` |  |
| STREAM-11 | P1 | `Flush` 失败时同样释放全部缓冲区并返回错误；`Discard` 不写出直接释放 | ✅ | `stream.FlushFailureAndDiscardRelease` |  |
| STREAM-12 | P1 | 只有 1 个缓冲区时直接 Write；`Flush` 零分配 | ✅ | `stream.SingleBufferUsesWriteAndFlushIsAllocFree` | 零分配断言只在非 race 构建下生效（NF-21） |
| STREAM-13 | P1 | 目标实现了 `WriteBuffers`（如 `*tcp.Conn`）时使用它；普通 io.Writer 退化为逐个写出，结果相同 | ✅ | `stream.BuffersWriterPreferredPlainWriterFallsBack` |  |
| STREAM-14 | P1 | `tcp.Conn.WriteBuffers` 透传 writev 到底层连接，数据完整到达 | ✅ | `tcp.ConnWriteBuffers` |  |
| STREAM-15 | P2 | 逐条 Write 与合并写出的对比（Benchmark） | ✅ | `stream.BenchmarkBacklog` | 经 `*tcp.Conn` 实测：积压 8 条快约 6 倍，32 条快约 9～10 倍，1 条与直接 Write 相同，均为零分配 |
| STREAM-18 | P0 | `AddShared` 与 `Add` 按调用顺序混排写出（writev 与逐个 Write 两种目标）；共享数据不被释放、不被改动；`Pending` 计入；Flush 后 Writer 不再引用任何已排队的切片；空切片忽略 | ✅ | `stream.AddSharedInterleavesWithoutReleasing` |  |
| STREAM-19 | P1 | 写失败和 `Discard` 时只释放 `Add` 的缓冲，共享数据不受影响，Writer 之后照常可用 | ✅ | `stream.AddSharedOnFailureAndDiscard` |  |
| STREAM-20 | P1 | 同一份 1MB 只读数据经 16 个连接并发发送，内容完整、无竞态、源数据不变；`AddShared` + `Flush` 零分配 | ✅ | `stream.SharedPayloadToManyConnections` | 游戏配置等广播场景 |
| STREAM-21 | P0 | 目标少写且不报错（违反 io.Writer 契约的包装）时，`Flush` 返回 `io.ErrShortWrite`，缓冲区照常全部释放；覆盖单个 Write、逐个 Write、`WriteBuffers` 三条路径 | ✅ | `stream.FlushShortWriteIsAnError` |  |

## 9. internal 与可观测性

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| SYS-01 | P1 | `ClassifyRead` 对各类 errno 的分类正确 | ✅ | `sys.ClassifyRead` | 仅 unix |
| SYS-02 | P1 | Windows 上的错误分类 | ✅ | `sys.ClassifyWinsock` | 策略写成纯函数，在所有平台上测试；Windows 上的实际读取路径未运行 |
| SYS-03 | P1 | `DropCount` 解析正确且零分配 | ✅ | `sys.DropCountDoesNotAllocate`<br>`sys.DropCountParse` | 仅 Linux |
| SYS-04 | P1 | `counter.Max` 在并发下单调不减 | ✅ | `counter.CounterMaxConcurrent`<br>`counter.CounterOperations` |  |
| SYS-05 | P0 | `BatchReader` 一次调用返回已排队的全部数据报（不等凑满），超过批量时分多次读出且顺序不变；每个数据报的长度、截断标志、来源地址、丢包计数各自正确；Close 能唤醒阻塞的 Read；Read 不分配内存 | ✅ | `sys.BatchReaderDoesNotAllocate`<br>`sys.BatchReaderReadsQueuedDatagrams` | 仅 Linux |
| SYS-06 | P1 | sockaddr 解析：IPv4、IPv6、IPv4 映射地址还原、链路本地地址的 zone（接口名，未知接口用数字）、未知地址族 | ✅ | `sys.BatchReaderAddressParsing` | 仅 Linux；用构造的 sockaddr 测试，当前环境没有 IPv6 网络 |
| OBS-01 | P0 | TCP Stats：Accepted、Active、Closed、Rejected | ✅ | `tcp.AdmitRejects`<br>`tcp.EchoAndStats` |  |
| OBS-02 | P1 | TCP Stats：AcceptErrors | ✅ | `tcp.TemporaryAcceptErrorsBackOffAndStopPromptly` |  |
| OBS-03 | P0 | UDP Stats：收发的包数和字节数 | ✅ | `udp.Echo` |  |
| OBS-04 | P1 | UDP Stats：Truncated、RecvErrors | ✅ | `udp.FatalReadErrorStopsServe`<br>`udp.TruncatedDropped` |  |
| OBS-05 | P1 | UDP Stats：SendErrors | ✅ | `udp.WriteAfterCloseIsErrClosedAndCounted` |  |
| OBS-06 | P1 | 读取 Stats 与更新同时进行时没有数据竞争 | ✅ | `tcp.StatsReadDuringTraffic` |  |
| OBS-07 | P2 | 关闭指标采集时不影响功能 | ➖ |  | 没有开关，计数器始终开启，只是一次原子加法 |

## 10. Runtime：生命周期

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| RUN-01 | P0 | 执行顺序：OnStart → Serve → Shutdown → OnStop | ✅ | `netx.OrderAndShutdown` |  |
| RUN-02 | P0 | Server 只能运行一次，第二次返回 `ErrAlreadyRunning` | ✅ | `netx.OneShotAndPanic` |  |
| RUN-03 | P0 | 父 ctx 取消时走 drain 流程，而不是直接 abort | ✅ | `netx.ParentCancelDrainsNotAborts` |  |
| RUN-04 | P0 | Serve 返回的错误会结束运行并被返回 | ✅ | `netx.ServeErrorStopsAndIsReturned` |  |
| RUN-05 | P1 | OnStart 失败时调用 Service.Shutdown 回收资源 | ✅ | `netx.StartErrorShutsDown` |  |
| RUN-06 | P1 | hook 中的 panic 被转换成错误 | ✅ | `netx.OneShotAndPanic` |  |
| RUN-07 | P1 | 服务卡住时最多等待 `AbortGrace` | ✅ | `netx.StuckServiceBoundedByAbortGrace` |  |
| RUN-08 | P1 | OnStop 有独立的 ShutdownTimeout 预算 | ✅ | `netx.OnStopGetsFreshContext`<br>`netx.OnStopHasIndependentTimeoutBudget` |  |
| RUN-09 | P1 | drain 期间收到第二次信号会立即 abort | ✅ | `netx.SecondSignalAborts` | 仅 unix |
| RUN-10 | P1 | 在运行前或停止后调用 Stop/Shutdown，返回 `ErrNotRunning` | ✅ | `netx.StopOutsideRunIsErrNotRunning` |  |
| RUN-11 | P1 | 并发调用 Stop 和 Shutdown | ✅ | `netx.ConcurrentStopAndShutdown` |  |
| RUN-12 | P1 | OnStop 返回的错误会被合并进最终结果 | ✅ | `netx.OnStopErrorIsReturned` |  |
| RUN-13 | P1 | `Group`：一个服务出错，其余服务被 abort | ✅ | `netx.GroupAbortsOthersOnError` |  |
| RUN-14 | P1 | `Group`：Shutdown 并发进行，错误合并返回 | ✅ | `netx.GroupShutdownJoinsErrors`<br>`netx.GroupWithRealTransports` |  |
| RUN-15 | P2 | 连续收到多次信号不会死锁 | ✅ | `netx.ConcurrentStopAndShutdown`<br>`netx.RepeatedSignalsDoNotDeadlock` | 仅 unix；信号之间留出间隔，避免内核合并同类信号 |
| RUN-16 | P2 | 停止后后台 goroutine 全部退出 | ✅ | `netx.StopReleasesGoroutinesAndSockets` | 用 internal/leak 检查 goroutine 和文件描述符 |
| RUN-17 | P1 | `OnError` 在每条退出路径上都恰好调用一次，包括 OnStart 失败；收到的错误与返回值相同，含 OnStart、Shutdown、OnStop 的全部错误；没有错误时不调用 | ✅ | `netx.OnErrorReceivesFinalErrorOnEveryPath` |  |
| RUN-18 | P1 | `OnError` 自身 panic 时被 recover：LoopContext 不 panic，原始错误仍可用 `errors.Is` 找到，panic 追加在后面 | ✅ | `netx.OnErrorPanicIsRecovered` |  |

## 11. 集成测试

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| INT-01 | P0 | Runtime 管理 TCP 服务，多个客户端同时通信 | ✅ | `integration.TCPLineEcho` |  |
| INT-02 | P0 | Runtime 通过 Group 同时管理 TCP 和 UDP | ✅ | `netx.GroupWithRealTransports` |  |
| INT-03 | P0 | UDP + Flow + Buf + Queue + Limit 按 Gateway 的方式组合 | ✅ | `integration.UDPGatewayComposition` |  |
| INT-04 | P1 | 在 `netxdebug` 下，整条链路零缓冲区泄漏 | ✅ | `integration.UDPGatewayComposition` |  |
| INT-05 | P1 | TCP + Flow：连接关闭时清理对应的 flow | ✅ | `integration.TCPFlowLifecycle` |  |
| INT-06 | P1 | 队列满时 DropOldest 释放缓冲区，不泄漏 | ✅ | `integration.FullQueueReleasesEvictedBuffers`<br>`integration.UDPGatewayComposition` |  |
| INT-07 | P1 | Budget 耗尽时丢包，恢复后继续正常工作 | ✅ | `integration.BudgetExhaustionDropsThenRecovers` |  |
| INT-08 | P1 | TCP 在 drain 期间，长连接收到 `Draining` 信号后主动退出 | ✅ | `integration.TCPDrainingGoAway` |  |
| INT-09 | P2 | 长时间运行，FD、内存、goroutine 都不泄漏 | ✅ | `integration.StressGatewayLongRun` | 压测，`make stress`；时长用 `NETX_STRESS_SECONDS` 设置，默认 3 秒，已跑过 15 秒 |

## 12. 回归清单与已知问题

Review 中发现的问题都要登记在这里，并对应到一个测试 ID。修复后状态改为 ✅，不要删除这一行。

| ID | 模块 | 问题 | 检查点 | 状态 |
|---|---|---|---|---|
| REG-01 | socket | `UnsupportedIgnore` 会吞掉所有 FastOpen setsockopt 错误，不只是“不支持”类错误 | SOCK-07 | ✅ |
| REG-02 | tcp | accept 的退避用 `time.Sleep`，Close 时无法被唤醒 | TCP-07 | ✅ |
| REG-03 | udp | 读错误退避无法被 Close 唤醒 | UDP-10 | ✅ |
| REG-04 | udp | `Reply(nil)` 会 panic | UDP-21 | ✅ |
| REG-05 | udp | Reply 时 Reader 越界会 panic | UDP-22 | ✅ |
| REG-06 | udp | 开启丢包计数的结果被忽略，无法区分“零丢包”和“没统计” | UDP-26、27 | ✅ |
| REG-07 | udp | `Readers` 没有上限 | UDP-13 | ❓ 见 D-01 |
| REG-08 | udp | `MaxPacket` 没有上限 | UDP-14 | ✅ 上限为 `MaxDatagram`（65000） |
| REG-09 | runtime | drain 耗尽时间后，OnStop 拿到的 ctx 已经过期 | RUN-08 | ✅ |
| REG-10 | runtime | 第二次信号被忽略 | RUN-09 | ✅ |
| REG-11 | flow | 只手动 Sweep 时会误清理活跃的 flow | FLOW-10 | ✅ |
| REG-12 | limit | Budget 在接近 `MaxInt64` 时溢出 | RES-13 | ✅ |
| REG-13 | limit | `Concurrency.Release` 多调用时，计数变负，凭空多出额度 | RES-04 | ✅ |
| REG-14 | limit | Budget 接受负数预留 | RES-11 | ✅ |
| REG-15 | flow | Get 与 Sweep 竞态：取出 flow 和刷新时间之间被清理 | FLOW-09 | ✅ |
| REG-16 | udp | 永久读错误会无限重试 | UDP-11 | ✅ |
| REG-17 | tcp | 永久 accept 错误时，等待长连接 handler 导致无法退出 | TCP-06 | ✅ |
| REG-18 | udp | `KernelDrops` 在 32 位回绕后停止增长 | UDP-28 | ✅ |
| REG-19 | socket | Dialer 开启 FastOpen 后，拿到 TFO cookie 的对端即使已关闭，Dial 也返回成功，错误到 Read 才出现；Dial 超时和 `DialEvent` 失效 | SOCK-28 | ✅ |
| REG-20 | socket | `socket.Dialer` 覆盖调用方的 `Control`；base 设置了 `ControlContext` 时，net 包忽略 `Control`，所有 socket 选项被静默跳过 | SOCK-29 | ✅ |
| REG-21 | udp | 多个 reader 共用一个 socket 时，默认 Batch 8 让一个 reader 一次取走多个包串行处理，其他 reader 空闲，并行度下降 | UDP-38 | ✅ |
| REG-22 | stream | 数据源一直返回 `(0, nil)` 时 `Peek` 死循环（实测 300ms 空转 8100 万次） | STREAM-16 | ✅ |
| REG-23 | stream | `Peek(-1)` 切片越界 panic；`Discard(-1)` 让读位置倒退，之后 `Buffered()` panic | STREAM-17 | ✅ |
| REG-24 | socket | `SetBuffers(nil, …)` 在缓冲区为 0 时返回 nil，大于 0 时 panic；typed nil 连接同样 panic | SOCK-30 | ✅ |
| REG-25 | socket | macOS FastOpen 测试无条件要求 `TCP_FASTOPEN` 非零，内核关闭 Fast Open 时，即使降级行为正确也会失败 | SOCK-26 | ✅ |
| REG-26 | runtime | OnStart 失败的退出路径不调用 `OnError`，与“接收最终错误”的约定不一致 | RUN-17 | ✅ |
| REG-27 | runtime | `OnError` 没有经过 `protect`，它 panic 时 LoopContext 直接 panic | RUN-18 | ✅ |
| REG-28 | flow | 调用方先 `Flow.Close()`，flow 再离开表时 `OnClose` 不会调用，违反“每个离开表的 flow 恰好一次” | FLOW-17 | ✅ |
| REG-29 | flow | `GetOrOpen` 的 newValue 或 `Range` 的 fn panic 后分片锁不释放，之后访问该分片永久阻塞 | FLOW-18、FLOW-19 | ✅ |
| REG-30 | stream | 目标少写且返回 nil 时 `Flush` 返回 nil 并释放缓冲区，调用方以为已发完；逐个 Write 路径上每个缓冲区只写出前一段，接收方拿到拼接错乱的流（实测 "abcd"、"efgh"、"ijkl" 写成 "abefij"） | STREAM-21 | ✅ |
| REG-31 | socket | macOS 在 listen 之前设置 `TCP_FASTOPEN` 返回 EINVAL，开启 FastOpen 的 Listen 直接失败（CI 在 macOS 26 上实测；PR #15 起一直存在，此前没有 macOS 实机运行） | SOCK-06、SOCK-26 | ✅ |
| REG-32 | udp | macOS 默认 `SO_SNDBUF` 为 9216 字节，超过它的数据报 `WriteTo` 返回 EMSGSIZE，`MaxDatagram` 的约定在 Mac 上不成立（CI 实测） | UDP-19、UDP-20 | ✅ |

### 设计决定

这里记录“看起来像问题，但经过讨论有意这样设计”的项，避免以后重复讨论。

| ID | 决定 | 理由 |
|---|---|---|
| D-01 | 不限制 `Readers` 的上限 | 规模决策交给调用方，文档建议不超过 CPU 核数 |
| D-02 | 收发统一使用 `MaxDatagram = 65000` | 收发规则一致；超过 65000 的包极少见，代价是这类包会被计入 Truncated |
| D-03 | UDP handler 没有 ctx，也不能被中断 | 换取零分配、无额外 goroutine；需要等待的工作交给 Gateway 的队列 |
| D-04 | `Concurrency.TryAcquire` 不阻塞 | Admission 只回答“现在能不能进”，排队属于 queue 的职责 |
| D-05 | Buf 的预算只统计正在使用的内存 | sync.Pool 中闲置的内存会被 GC 回收，无法也不需要计入 |
| D-06 | 暂不把 Sweep 改为增量式（待定） | 100 万个 flow 每次 Sweep 实测 19～105ms（随机器不同，见 FLOW-14），按 1 秒一次约占一个核 2%～10%，按上限估算。目前各 Gateway 的规模远小于此，改为增量会增加复杂度；flow 数量接近百万时再改为每次只扫一部分分片 |
| D-07 | TCP 不使用 worker 池复用 goroutine | worker 池是 fasthttp 针对 Web 应用大量短连接的优化；go-netx 面向长连接，新连接很少。实测每个连接只能省约 3µs（新开 goroutine 约 3.5～4.2µs 并有 1 次分配，复用约 0.6µs），约占一次 accept（79µs）的 4%，抵不过管理空闲 worker 增加的复杂度。只有当某个 Gateway 需要处理每秒数万个短连接时，才用该场景的 Benchmark 重新评估 |
| D-08 | 拒绝连接时不回复协议层消息（不像 fasthttp 回 503） | 传输层不理解协议；需要先回复再关闭的 Gateway 自行在准入逻辑中处理 |
| D-09 | 不提供每连接独立的 context（不像 net/http 的 ConnContext） | 每个连接要多一次分配，而 Gateway 很少需要；共享的值来自 Serve 的 ctx，连接身份用 `Conn.ID()` |
| D-10 | `MaxConnsPerIP` 只做单机限制，不做分布式 | 准入检查跑在 accept 循环里，必须本地、不阻塞；负载均衡按源 IP 哈希时，单机限制就等于全局限制。按用户、按会话的全局限制属于 Gateway 业务层；部署在 L4 负载均衡后面时，要确保拿到真实客户端 IP |
| D-11 | 保持“每连接一个 goroutine”的模型，不自建 epoll（不像 netpoll） | 实测每个空闲连接约 3KB（栈约 2KB + 堆约 1KB），10 万连接约 300MB，可以接受；换来 TLS、跨平台、`net.Conn` 生态和零依赖。单机连接数达到 50 万到 100 万量级时，在该 Gateway 中直接使用 netpoll 或 gnet，而不是在 go-netx 里重新实现 |
| D-12 | `stream` 作为独立的包，与 tcp、udp 同级 | `buf` 只负责内存，`tcp` 是不涉及缓冲的传输层，读写缓冲放在哪一个里都会破坏它原有的职责；只对字节流有意义，UDP 用不上 |
| D-13 | 删除 `UnsupportedPolicy` 和 `UnsupportedIgnore`（原决定为保留，已改为删除） | 不再提供由调用方选择“报错还是忽略”的开关。不支持时怎么办由选项本身的性质决定，规则见 D-18；影响行为的选项不支持时一律报错，与 D-14、D-16 一致 |
| D-14 | 选项不适用于当前网络类型时一律报错 | 在 unix 等网络上设置选项、在 UDP 上设置 FastOpen 或 KeepAlive，都属于配置错误，不再静默忽略。代价：TCP 和 UDP 不能再共用一份带 TCP 专属选项的 `socket.Options` |
| D-15 | macOS 支持 ReusePort，FreeBSD 使用 `SO_REUSEPORT_LB` | macOS 允许多个 socket 绑定同一端口，但不分流，只有一个能收到数据；支持它是为了让同一份配置能在 Mac 开发机上运行，文档中写明不分流。FreeBSD 的 `SO_REUSEPORT_LB` 与 Linux 一样分流 |
| D-16 | 缓冲区小于请求值时报错，不只报告 | 内核截断缓冲区时不报错（Linux 截到 rmem_max），网关会带着比设计小的缓冲区运行，直到高峰期丢包才发现。启动时报错，并提示调大 sysctl 或授予 CAP_NET_ADMIN，与“不静默忽略”的原则一致。代价：默认配置的机器上，请求大缓冲区会启动失败 |
| D-17 | Linux 上默认用 recvmmsg 批量读取（`Batch` 默认 8） | 实测有积压时每个数据报的接收开销降低约 44%，且不增加延迟（不等凑满）。代价是每个 reader 的内存变为 Batch × MaxPacket，默认约 520KB；小包网关应设置 MaxPacket。32 比 8 提升很小但内存翻 4 倍，所以默认取 8 |
| D-18 | 只影响性能的选项在平台做不到时照常运行，影响行为的选项做不到时报错 | 要求同一份配置在 Mac 开发机和 Linux 上都能运行。监听端 FastOpen（省一个往返）、`Batch`（每次系统调用收几个包）、macOS 上的 ReusePort（能绑定但不分流）只影响性能，做不到时按普通方式运行，并由 `socket.Supported()`、`udp.Conn.Batch()` 报告实际情况；能做到的平台尽量真实现（macOS 监听端 FastOpen）。不适用于当前网络类型的选项（D-14）、达不到请求大小的缓冲区（D-16）、其他平台上的 ReusePort（第二次绑定会失败）会改变行为，仍然报错 |
| D-19 | 拨出方向不支持 FastOpen，Dialer 设置了就报错 | 实测 Linux 上拿到 TFO cookie 后，用 FastOpen 拨一个已关闭的端口，Dial 约 100µs 返回成功，Write 也成功，错误到 Read 才出现，Dial 超时和 `DialEvent` 不再反映对端是否可达，网关会把挂掉的上游当成可用。这会改变行为，不属于 D-18 的性能项。网关拨上游通常不需要省这一个往返；监听端 FastOpen 保留 |
| D-20 | `Get` 只查找，`Touch` 是唯一更新 LastSeen 的操作 | 查找时顺带刷新时间，会让后台查询、`Range`、管理操作把空闲 flow 一直续命。拆开后，空闲超时只反映网关显式记录的真实流量。查找与 Touch 之间被 Sweep 插入的竞态（REG-15）改用“LastSeen 加冻结位”的单字 CAS 解决。代价：网关在收到数据时要自己调用 `Touch` 或 `GetAndTouch`；`Get` 因此更快 |
| D-21 | buf 改为 2 的幂分档（64B…64KB，共 11 档），取代原来的 2KB、16KB、64KB 三档 | 三档最多浪费 8 倍，预算按容量扣，等于同样的预算少装好几倍数据（3KB 的消息占 16KB，50B 的包占 2KB）。主流 Go 缓冲池（netpoll 用的 mcache、fasthttp 用的 bytebufferpool、smux、gnet、libp2p）都按 2 倍分档。实测档位增多不影响速度（Get + Release 仍约 9ns，0 分配）。下限 64B：再小相对 `Buf` 本身的开销省不了多少 |

## 12.1 文档一致性

| ID | P | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| DOC-01 | P0 | 本文档与测试代码一一对应（规则见 0.1 节） | ✅ | `main.DocMatchesTests` |  |
| DOC-02 | P1 | 每个包在 `docs/design/` 下都有设计文档：根包对应 runtime.md，internal 下的子包在 internal.md 中各有一节，其余包对应 `<包名>.md` | ✅ | `main.DocMatchesTests` |  |

## 13. 非功能测试

| ID | 类型 | 检查点 | 状态 | 测试 | 说明 |
|---|---|---|---|---|---|
| NF-01 | Race | 所有包 | ⚙️ |  | `go test -race ./...` |
| NF-02 | Race | Debug 构建 | ⚙️ |  | `go test -race -tags netxdebug ./...` |
| NF-21 | Test | 不带 race 的一轮测试，分配次数断言只在这一轮生效 | ⚙️ |  | `make test`，已包含在 `make verify` 中 |
| NF-03 | Stress | TCP 大量并发连接 | ✅ | `tcp.StressTCPChurn` | `make stress` |
| NF-04 | Stress | UDP 高并发读写，并真正触发内核丢包 | ✅ | `udp.StressUDPEchoManySenders` | `make stress` |
| NF-05 | Stress | Flow 并发创建与清理 | ✅ | `flow.ActiveFlowSurvivesSweep`<br>`flow.ConcurrentRemovalClosesFlowOnce`<br>`flow.GetOrOpenConcurrentSingleCreation`<br>`flow.StressTableChurn` | `make stress` |
| NF-06 | Stress | Queue 多生产者、多消费者 | ✅ | `queue.ConcurrentNoLoss`<br>`queue.MultiConsumerNoLossNoDuplicate` |  |
| NF-07 | Fuzz | `DropCount` 解析任意字节不会 panic | ✅ | `sys.FuzzDropCount` | 种子语料随 `go test` 运行；`make fuzz` 对每个目标做长时间 fuzz |
| NF-08 | Fuzz | UDP 的 Reply 和 WriteTo 接收任意参数都不会 panic，只返回文档列出的错误 | ✅ | `udp.FuzzReplyAndWriteTo` | 种子语料随 `go test` 运行；`make fuzz` 对每个目标做长时间 fuzz |
| NF-09 | Fuzz | Flow 表的随机操作序列与模型一致，epoch 有效，OnClose 恰好一次 | ✅ | `flow.FuzzTableOps` | 种子语料随 `go test` 运行；`make fuzz` 对每个目标做长时间 fuzz |
| NF-10 | Fuzz | Queue 的随机操作序列与模型一致：FIFO、容量、溢出策略、Stats | ✅ | `queue.FuzzRingOps` | 种子语料随 `go test` 运行；`make fuzz` 对每个目标做长时间 fuzz |
| NF-11 | Fuzz | Buf 的随机 Get/Release 序列：预算与持有量一致，内存不共享 | ✅ | `buf.FuzzGetRelease` | 种子语料随 `go test` 运行；`make fuzz` 对每个目标做长时间 fuzz |
| NF-12 | Bench | UDP 回显 | ✅ | `udp.BenchmarkEcho` | 约 10µs，0 次分配 |
| NF-22 | Bench | UDP 接收（队列中有积压时每个数据报的接收开销），按 Batch 1、8、32 对比 | ✅ | `udp.BenchmarkRecv` | 2 vCPU：Batch 1 约 2.2µs，Batch 8 约 1.2µs，0 次分配 |
| NF-13 | Bench | TCP accept 吞吐 | ✅ | `tcp.BenchmarkAccept` | 实测约 79µs/连接（建立、接受、关闭），23 次分配 |
| NF-14 | Bench | 不同 flow 数量下的查找和清扫 | ✅ | `flow.BenchmarkGetAddrPort`<br>`flow.BenchmarkGetAndTouch`<br>`flow.BenchmarkGetBySize`<br>`flow.BenchmarkGetOrOpenExisting`<br>`flow.BenchmarkSweepBySize` | `flow.BenchmarkGetBySize`、`flow.BenchmarkSweepBySize` |
| NF-15 | Bench | Queue | ✅ | `queue.BenchmarkPushPop` |  |
| NF-16 | Bench | Buf | ✅ | `buf.BenchmarkGetReleaseBySize`<br>`buf.BenchmarkGetRelease` |  |
| NF-19 | Bench | 共享计数器 | ✅ | `counter.BenchmarkCounterInc` |  |
| NF-20 | Fuzz | stream.Reader：随机的读取分块与 Peek/Discard 序列与模型一致 | ✅ | `stream.FuzzReader` | `make fuzz` |
| NF-17 | 交叉编译 | linux/amd64、linux/386、darwin/arm64、windows/amd64、freebsd/amd64、js/wasm | ⚙️ |  | 对 linux/amd64、linux/386、darwin/arm64、windows/amd64、freebsd/amd64、js/wasm 执行 `GOOS=… GOARCH=… go vet ./...` |
| NF-18 | 泄漏 | 生命周期结束后 goroutine 和 FD 被回收 | ✅ | `leak.DetectsLeakedGoroutineAndFD` | `internal/leak`：检查 goroutine 和文件描述符（Linux），不引入外部依赖 |

## 14. 网络库通用问题清单

这一节和 go-netx 的具体 API 无关，总结的是开发网络库时反复出现的问题类型。写新的网络库，或者审查别人的网络库时，可以逐项检查。括号里是 go-netx 中对应的实际案例。

### 14.1 等待与关闭

- [ ] 所有退避、重试的等待都能被关闭唤醒，不能用不可中断的 `time.Sleep`（REG-02、REG-03）。
- [ ] 关闭操作能唤醒所有等待者：消费者、生产者，以及阻塞在 I/O 上的 goroutine（QUEUE-08、QUEUE-09）。
- [ ] “停止接收新请求”和“取消正在处理的请求”是两个独立的阶段，handler 的 ctx 只在 abort 时取消（TCP-16）。
- [ ] 每个关闭阶段都有独立的超时预算，后一阶段不会继承前一阶段已经过期的 ctx（REG-09）。
- [ ] 卡住的组件不会让整个进程无法退出：有兜底超时，第二次信号能强制退出（RUN-07、REG-10）。
- [ ] 永久性错误会主动终止并向上报告，不会无限重试，也不会让服务处于“看起来正常、实际已经不可用”的状态（REG-16、REG-17）。

### 14.2 资源上限的算术

- [ ] 拒绝负数输入（REG-14）。
- [ ] 做加法之前先检查是否会溢出，不要“先加上去，超了再减回来”（REG-12）。
- [ ] 失败的尝试不会让计数短暂超过上限，要用 CAS 而不是乐观地加减（RES-03）。
- [ ] 释放次数多于申请次数时要报错，不能悄悄多出额度（REG-13）。
- [ ] 内核提供的 32 位计数器会回绕，要按增量累加；多个读者乱序上报的旧值不能重复累加（REG-18）。

### 14.3 所有权与泄漏

- [ ] 每块资源在任何时刻都只有一个主人，借用、持有、转移三种状态都写进文档。
- [ ] 转移失败时，所有权仍归原主人（QUEUE-03）。
- [ ] 丢弃路径（队列溢出、关闭时剩余的元素、错误分支）都会释放资源，这是最容易泄漏的地方（QUEUE-04、QUEUE-12）。
- [ ] 提供调试构建，能检测重复释放并统计未释放的数量（BUF-05、BUF-06）。
- [ ] 关闭之后，goroutine 和文件描述符都要回到基线，要有测试检查，在压测之后也要检查（RUN-16、NF-18）。
- [ ] 传给回调的数据是借用的，回调返回后就失效；要保留必须先复制。

### 14.4 并发与竞态

- [ ] 清理任务可能插在“查找”和“更新状态”之间（REG-15）：要么在同一把锁内完成，要么像 `flow.Touch` 那样用一次 CAS 判定先后，并让输的一方明确失效。
- [ ] 后台任务推进的共享时钟，在手动调用时也要推进（REG-11）。
- [ ] 关闭和新请求之间的竞态窗口：刚 accept 进来的连接，在关闭时也要被登记或释放（TCP-19）。
- [ ] 回调不要在持锁时调用，或者在文档中明确写出持锁回调的约束。
- [ ] 共享 socket 的多个读者在更新同一份统计时，要考虑乱序。

### 14.5 输入与 API 边界

- [ ] 公开类型的字段可能被调用方伪造，公开方法遇到非法值时返回错误，不要 panic（REG-04、REG-05）。
- [ ] 配置项都要确定 0、负数和上限各自的含义，并写进文档（UDP-13、UDP-14）。
- [ ] 没有意义的配置值直接拒绝，比如超过协议上限的缓冲区大小。
- [ ] 能力检测的结果要对外暴露，不能让“不支持”看起来像“零”（REG-06）。
- [ ] 平台不支持某个选项时，先判断它只影响性能还是会改变行为：只影响性能的照常运行，并能查到实际是否生效；会改变行为的必须显式失败。不提供由调用方选择的忽略开关（D-13、D-18）。

- [ ] fuzz 中执行速度降为“0 次每秒”，不一定是卡死：发现新输入后，引擎会花最长 60 秒去最小化它。先用 `-fuzzminimizetime` 缩短再判断。测试框架本身要保持线性复杂度，否则大输入会让它慢得像卡死。

### 14.6 平台差异

- [ ] 同一种语义在不同平台上可能以不同方式出现，比如截断：Linux 设置 MSG_TRUNC 标志，Windows 返回错误（UDP-06）。
- [ ] ICMP 错误在 Windows 上会以读错误的形式出现在未连接的 UDP socket 上，要归为暂时性错误。
- [ ] socket 选项要在 bind 或 listen 之前设置，才能被 accept 出来的连接继承，TCP 窗口缩放也才能生效（SOCK-03、SOCK-04）。
- [ ] 内核附在数据报上的统计（比如 SO_RXQ_OVFL）只会随下一个入队的包一起到达，流量停下来时读到的值会偏小；文档要写明，测试要补发一个探测包再对账（UDP-31）。
- [ ] 每个声明支持的平台都要交叉编译通过；只在某个平台上运行的测试，用构建标签隔离（NF-17）。

### 14.7 性能

- [ ] 包装了 `net.Conn` 的类型会让 `net.Buffers` 识别不出，writev 会静默退化成逐个写出；包装类型要显式提供 writev 透传（STREAM-13、STREAM-14）。不能用“拆开包装、直接写底层连接”的办法，对 TLS 连接会绕过加密。
- [ ] 零分配之类的断言在 race 模式下会被跳过（sync.Pool 会故意丢弃对象），必须另外跑一轮不带 race 的测试（NF-21）。
- [ ] 借鉴其他库的优化之前，先确认对方面向的负载和自己是否相同。例如 fasthttp 面向大量短连接，它的 worker 池对长连接网关几乎没有收益（D-07）。
- [ ] 热路径不创建 goroutine、不分配内存、不经过内部队列，并用 Benchmark 断言分配次数。
- [ ] 按核数分配的内存可以预先分配，按连接数分配的内存要用池，绝不按包分配。
- [ ] 共享计数器按 cache line 对齐，或者按读者分片，避免伪共享。
- [ ] 全表扫描的成本随规模增长，而且超出 CPU 缓存后会更快地上升，要用 Benchmark 测出不同规模下的数字（FLOW-14）。
- [ ] 文档中的性能数字必须来自实测，不能估算。go-netx 曾两次犯这个错误：把 Sweep 的成本写成每百万约 20ms（当时实测约 105ms；后来在另一台机器上实测 19～50ms，可见这类数字还要写明机器和波动范围），把新开 goroutine 的开销写成“微秒以下”（实测约 3.5～4.2µs）。

## 15. 测试执行分层

| 层次 | 命令 | 何时执行 |
|---|---|---|
| 单元测试 | `go test ./...` | 每次提交 |
| Race | `make race`（包含 `-race` 和 `-race -tags netxdebug` 两轮） | 每个 PR |
| Benchmark | `make bench` | 涉及性能的改动前后各跑一次，对比结果 |
| 交叉编译 | 对 NF-17 列出的每个平台执行 `GOOS=… GOARCH=… go vet ./...` | 每个 PR 和每次发布 |
| Fuzz | `make fuzz`（默认每个目标 30 秒，可用 `FUZZTIME=5m` 调整） | 改动解析、队列、flow、buf 的代码后，以及发布前 |
| Stress / 泄漏 | `make stress`（`stress` 构建标签，普通构建和 `netxdebug` 各跑一轮；`NETX_STRESS_SECONDS` 控制长跑时长） | 合并前，以及发布前用较长时长再跑一次 |

## 16. 更新规则

以下是本文档自身的维护规则，完整规则见 [CONTRIBUTING.md](CONTRIBUTING.md)。

1. **新增公开 API**：同一个 PR 里要在对应模块下加上检查点，状态不能是空白。
2. **新增测试**：在函数上写 `// Checks: <ID>`，把对应检查点的状态改为 ✅ 或 ⚠️，然后运行 `go run ./internal/tools/testdoc -w`。
3. **Review 发现问题**：在第 12 节登记一个 REG 编号，并对应到一个检查点；修复后状态改为 ✅，不要删除这一行。
4. **讨论后决定不修改**：登记到“设计决定”，写明理由。
5. **ID 不复用**：检查点删除后，它的 ID 也不再分配给其他检查点。
6. **第 14 节只放通用经验**：只和 go-netx 有关的内容放在前面各模块的章节里。
