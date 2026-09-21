# challenge-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/challenge-kit.svg)](https://pkg.go.dev/github.com/soulteary/challenge-kit)
[![Go Report Card](.github/goreportcard.svg)](.github/goreportcard-report.md)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![codecov](https://codecov.io/gh/soulteary/challenge-kit/graph/badge.svg)](https://codecov.io/gh/soulteary/challenge-kit)

[English](README.md)

管理 OTP（一次性密码）验证的 Go 库：生命周期、验证码生成、在单挑战锁保护下的 Argon2
校验、尝试次数统计与用户锁定，全部以 Redis 为后端。

## 特性

- **挑战生命周期**：创建、验证、撤销，以及切换当前唯一有效挑战
- **验证码生成**：由 `crypto/rand` 产生的数字验证码
- **校验**：Argon2 哈希 + 常量时间比较
- **尝试计数原子**：并发的错误验证码各自恰好消耗一次尝试
- **失败即关闭**：Redis 故障绝不会报告成功
- **工作量有界**：并发 Argon2 比较有上限，验证无法耗尽内存
- **用户锁定**：每个挑战最多建立一次，反复探测无法延长
- **用途绑定**：为某一用途签发的挑战无法被另一用途兑换
- **隐私**：活跃索引键是不可逆摘要，绝不含原始 PII
- **任意 Redis 部署形态**：单机、Sentinel、Cluster、Ring 客户端都能直接传入

## 要求

- **Go 1.27+**（`go.mod` 声明 `go 1.27.0`）
- Redis，通过 `github.com/redis/go-redis/v9` —— 单机、Sentinel、Cluster 或 Ring
- `github.com/soulteary/secure-kit/v2`

## 安装

```bash
go get github.com/soulteary/challenge-kit
```

## 快速开始

```go
package main

import (
    "context"
    "errors"
    "log"

    "github.com/redis/go-redis/v9"
    challenge "github.com/soulteary/challenge-kit"
)

func main() {
    redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
    defer redisClient.Close()

    manager := challenge.NewManager(redisClient, challenge.DefaultConfig())
    ctx := context.Background()

    ch, code, err := manager.Create(ctx, challenge.CreateRequest{
        UserID:      "user123",
        Channel:     challenge.ChannelEmail,
        Destination: "user@example.com",
        Purpose:     "login",
        ClientIP:    "127.0.0.1",
    })
    if err != nil {
        log.Fatal(err)
    }

    // 通过所选渠道把 code 发给用户，然后：
    result, err := manager.Verify(ctx, ch.ID, code, "127.0.0.1")
    switch {
    case errors.Is(err, challenge.ErrLockUnavailable):
        // 可重试。没有消耗尝试次数 —— 不要当成验证码错误上报。
        return
    case errors.Is(err, challenge.ErrBackendUnavailable):
        // Redis 不健康；manager 已失败即关闭。
        return
    case err != nil:
        log.Fatal(err)
    }

    if result.OK {
        // 认证通过。挑战已被删除。
        return
    }

    switch result.Reason {
    case challenge.ReasonInvalid:       // 验证码错误
    case challenge.ReasonExpired:       // 挑战 TTL 已过
    case challenge.ReasonLocked:        // 尝试耗尽，用户刚被锁定
    case challenge.ReasonUserLocked:    // 用户此前已被锁定
    case challenge.ReasonLockContention: // 可重试，未消耗尝试次数
    case challenge.ReasonContextMismatch: // 用途/用户/渠道绑定校验失败
    case challenge.ReasonBackendUnavailable:
    }
    if result.RemainingAttempts != nil {
        log.Printf("还剩 %d 次尝试", *result.RemainingAttempts)
    }
}
```

## 使用

### 处理验证结果

`Verify` 把三件调用方绝不能混为一谈的事情区分开了：

| 情形 | 信号 | 消耗尝试？ | 是否重试 |
|------|------|-----------|----------|
| 验证码错误 | `Reason == ReasonInvalid` | 是 | 不重试，让用户重新输入 |
| 尝试耗尽 | `Reason == ReasonLocked` | 是 | 不重试，用户已被锁定 |
| 用户已被锁定 | `Reason == ReasonUserLocked` | 否 | `LockoutDuration` 之后 |
| 锁竞争 | `ErrLockUnavailable`、`Reason == ReasonLockContention` | **否** | **是，立即重试** |
| Redis 故障 | `ErrBackendUnavailable`、`Reason == ReasonBackendUnavailable` | 否 | 是，带退避 |
| 绑定不匹配 | `Reason == ReasonContextMismatch` | 是 | 不重试 |

`ErrLockUnavailable` 是一次瞬时重试，不是验证失败。把它当作失败会消耗用户根本没花掉
的一次尝试，而对它显示"您的账号已被锁定"则完全是错的。

### 把挑战绑定到用途

`VerifyWithOptions` 在单挑战锁内、比较验证码之前完成绑定校验，因此为 `"login"`
签发的验证码绝不可能被 `"password_reset"` 兑换。

```go
result, err := manager.VerifyWithOptions(ctx, challengeID, code, clientIP,
    challenge.VerifyOptions{
        ExpectedPurpose: "login",
        ExpectedUserID:  "user123",
        ExpectedChannel: challenge.ChannelEmail,
    })
// 绑定失败时 result.Reason == challenge.ReasonContextMismatch
```

所有字段都是可选的，留空即不校验。

### 单一有效挑战（两阶段发送）

`Create` 把挑战存为*待定*状态。只有在服务商确认已接收消息之后，才把它置为有效，
这样发送失败就不会留下一个可兑换的验证码：

```go
ch, code, err := manager.Create(ctx, req)
if err != nil {
    return err
}

if err := smsProvider.Send(ch.Destination, code); err != nil {
    // 什么都没发出去 —— 删除待定挑战，保留原来的有效挑战。
    _ = manager.RevokePending(ctx, ch.ID)
    return err
}

previousID, err := manager.SwapActive(ctx, ch)
if err != nil {
    return err
}
if previousID != "" {
    _ = manager.Revoke(ctx, previousID) // 退役被它替换掉的那个挑战
}
```

`SwapActive` 是原子的，遇到 Redis 错误会失败即关闭。它的索引键由身份的不可逆摘要
导出，绝不使用原始手机号或邮箱。`RevokePending` 不触碰活跃索引，`Revoke` 会。

### 锁定状态

```go
if manager.IsUserLocked(ctx, "user123") {
    // 提前拒绝，不消耗挑战。
}
```

锁定在每个挑战上最多建立一次，记录在 `Challenge.LockoutApplied`。反复探测一个已耗尽
的挑战无法延长锁定。

### 该传哪种 Redis 客户端

`NewManager` 接受的是 `RedisClient` —— manager 实际会用到的七条命令，而不是某个具体客户端：

```go
type RedisClient interface {
    Del(ctx context.Context, keys ...string) *redis.IntCmd
    Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
    Exists(ctx context.Context, keys ...string) *redis.IntCmd
    Get(ctx context.Context, key string) *redis.StringCmd
    Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
    SetArgs(ctx context.Context, key string, value interface{}, a redis.SetArgs) *redis.StatusCmd
    TTL(ctx context.Context, key string) *redis.DurationCmd
}
```

`*redis.Client`、`*redis.ClusterClient`、`*redis.Ring` 和 `redis.UniversalClient`
都满足它，因此各种部署形态都能用：

```go
// 单机（写法不变）
manager := challenge.NewManager(redis.NewClient(&redis.Options{Addr: "localhost:6379"}), cfg)

// Sentinel
manager = challenge.NewManager(redis.NewUniversalClient(&redis.UniversalOptions{
    MasterName: "mymaster",
    Addrs:      []string{"10.0.0.1:26379", "10.0.0.2:26379"},
}), cfg)

// Cluster
manager = challenge.NewManager(redis.NewClusterClient(&redis.ClusterOptions{
    Addrs: []string{"10.0.0.1:6379", "10.0.0.2:6379"},
}), cfg)
```

每条命令都**只涉及单个键**，不需要任何键落在同一个 hash slot：集群可以把挑战键、
用户锁、验证锁和活跃索引放在任意分片上。

既然是接口，你自己的类型同样可以传入 —— 包一层加埋点、加链路追踪，或者在测试里换成假实现。

nil 客户端不会被解引用，其中也包括**带类型的 nil**（例如没有赋值的 `*redis.Client`
字段）：接口会照单全收，随后在调用命令时 panic。这里改为各操作返回 `ErrNilClient`，
`Verify` 以 `ReasonBackendUnavailable` 失败即关闭，`IsUserLocked` 返回 `true`。

### 面向接口测试

`ManagerInterface` 覆盖了 `Manager` 的全部方法，便于注入替身：

```go
type svc struct{ challenges challenge.ManagerInterface }
```

### 辅助函数

```go
code, err := challenge.GenerateCode(6)            // 数字，crypto/rand
ok := challenge.ValidateCodeFormat(code, 6)       // 纯数字且长度正确
```

当系统 CSPRNG 无法读取时，`GenerateCode` 返回包装了 `ErrEntropyUnavailable` 的错误，
而不是返回一个可猜测的验证码。

## 配置

```go
config := challenge.DefaultConfig()
config.Expiry = 5 * time.Minute
config.MaxAttempts = 5
config.MaxConcurrentVerifications = 16

manager := challenge.NewManager(redisClient, config)
```

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `Expiry` | `5m` | 挑战 TTL，由 Redis 执行 |
| `MaxAttempts` | `5` | 锁定前允许的错误次数 |
| `LockoutDuration` | `10m` | 被锁定用户的锁定时长 |
| `CodeLength` | `6` | 1–10 位数字 |
| `ChallengeKeyPrefix` | `"otp:ch:"` | 挑战的 Redis 键前缀 |
| `LockKeyPrefix` | `"otp:lock:"` | 用户锁定的 Redis 键前缀 |
| `VerifyLockPrefix` | `"otp:vlock:"` | 单挑战锁的 Redis 键前缀 |
| `VerifyLockTTL` | `5s` | 验证期间持有的租约 |
| `VerifyLockWait` | `2s` | 等待获取锁的总时长 |
| `VerifyLockRetry` | `25ms` | 两次取锁尝试的间隔 |
| `ActiveIndexPrefix` | `"otp:active:"` | 活跃挑战索引的 Redis 键前缀 |
| `MaxConcurrentVerifications` | `16` | 并发 Argon2 比较数 |

`DefaultConfig()` 会为每个字段填上 `Manager` 真正会用的值，读回来是真实默认值而不是零值。

**如何设置 `MaxConcurrentVerifications`**：每个在途验证都占着 Argon2 的内存成本
——库默认是 64 MiB——所以这个上限同时也是内存上限。默认 16 把验证内存约束在约 1 GiB。
只有在你有 `MaxConcurrentVerifications × Argon2 内存` 的余量时才提高它。

## API 参考

### Manager

| 方法 | 说明 |
|------|------|
| `Create(ctx, req)` | 存入待定挑战，返回挑战与明文验证码 |
| `Verify(ctx, id, code, clientIP)` | 校验验证码 |
| `VerifyWithOptions(ctx, id, code, clientIP, opts)` | 带用途/用户/渠道绑定的校验 |
| `Get(ctx, id)` | 取出挑战 |
| `Revoke(ctx, id)` | 删除挑战并清除活跃索引 |
| `RevokePending(ctx, id)` | 删除发送失败的挑战，不动索引 |
| `SwapActive(ctx, ch)` | 把 `ch` 置为有效，返回此前有效的 ID |
| `IsUserLocked(ctx, userID)` | 用户当前是否被锁定 |

### 类型

```go
type Challenge struct {
    ID             string
    UserID         string
    Channel        Channel // "sms" | "email" | "dingtalk"
    Destination    string  // 原始手机号/邮箱 —— 见"安全说明"
    CodeHash       string
    Purpose        string
    ExpiresAt      time.Time
    Attempts       int
    MaxAttempts    int
    CreatedIP      string
    CreatedAt      time.Time
    LockoutApplied bool // 该挑战已经触发过一次锁定
}

type CreateRequest struct {
    UserID      string
    Channel     Channel
    Destination string
    Purpose     string
    ClientIP    string
}

type VerifyOptions struct {
    ExpectedPurpose string
    ExpectedUserID  string
    ExpectedChannel Channel
}

type VerifyResult struct {
    OK                bool
    Challenge         *Challenge
    Reason            string // Reason* 常量之一
    RemainingAttempts *int
}
```

### Reason 常量

| 常量 | 取值 |
|------|------|
| `ReasonInvalid` | `"invalid"` |
| `ReasonExpired` | `"expired"` |
| `ReasonLocked` | `"locked"` |
| `ReasonUserLocked` | `"user_locked"` |
| `ReasonLockContention` | `"lock_contention"` |
| `ReasonContextMismatch` | `"context_mismatch"` |
| `ReasonBackendUnavailable` | `"backend_unavailable"` |

### 错误

| 哨兵错误 | 含义 |
|----------|------|
| `ErrLockUnavailable` | 在预算时间内没能拿到单挑战锁。可重试，**未消耗尝试次数**。绝不要当作验证失败，也绝不要退回非原子路径。 |
| `ErrBackendUnavailable` | 必需的 Redis 操作失败。manager 失败即关闭，后端不健康时绝不报告 OK。 |
| `ErrEntropyUnavailable` | 系统 CSPRNG 无法读取。返回此错误而不是签发可猜测的 ID 或验证码。 |
| `ErrNotFound` | 键不存在或已过期，与后端故障相区分。未命中同时满足 `errors.Is(err, redis.Nil)`。 |
| `ErrNilClient` | 构造 manager 时没有拿到可用的 Redis 客户端。 |

请用 `errors.Is` 判断。

## 升级说明（v1.9.0）

纯增量。没有删除任何 API，模块路径不变，现有调用照常编译。实测数字见
[CHANGELOG.md](CHANGELOG.md)。

- **`NewManager` 的参数从 `*redis.Client` 改为 `RedisClient`。** 继续传
  `*redis.Client` 依然编译得过；Cluster、Sentinel、Ring 客户端现在也能传了。唯一需要
  改的是把 `NewManager` 存进
  `func(*redis.Client, challenge.Config) *challenge.Manager` 类型变量的代码。
- **不再依赖 `redis-kit`。** 正是它的 cache 把客户端类型钉死成具体类型
  —— `cache.NewCache` 只接受 `*redis.Client` —— 所以那几十行改为放在本包内实现。
  使用方的 `go.sum` 少两行，模块图少一个模块。留一个废弃的 shim 并不可行：shim 必须
  import `redis-kit` 才能写出它的类型，模块又会被拉回来。
- **未命中改用 `challenge.ErrNotFound` 判定。** 如果你此前用
  `errors.Is(err, cache.ErrKeyNotFound)` 匹配 redis-kit 的哨兵，请改成
  `challenge.ErrNotFound`；`redis.Nil` 仍然匹配，错误文案也没变。
- **nil 客户端失败即关闭，不再 panic**，包括旧的具体类型参数根本表达不出来的带类型 nil。
- **`secure-kit` 从 v1.6.0 升到 v2.0.0**，其 Argon2 哈希器已移入 `passwd` 子包。哈希本身
  没有任何变化 —— v1.6.0 写出的哈希在 v2.0.0 下可以校验通过，反之亦然，因此滚动发布不会
  让在途挑战失效 —— 而且本包的 API 里不出现任何 secure-kit 类型，升级对调用方不可见。

## 升级说明（v1.8.0）

仅升级依赖。没有删除任何 API，调用方无需改代码。

- 测试用 Redis 为 `miniredis` v2.39.0（此前 v2.36.1）。
- 仍依赖 `redis-kit` v1.6.0 与 `secure-kit` v1.6.0。这两个模块没有更新的已发布版本。

## 升级说明（v1.7.0）

本次发布改变了"验证失败"告知调用方的内容，并给验证能消耗的资源加了上限。`Config`
和 `Challenge` 各新增一个字段，没有删除任何 API。

- **`Reason` 不再把"重试"和"锁定"混在一起。** 锁竞争（可重试、不消耗尝试）与尝试
  耗尽（终态、账号已锁定）此前都报 `"locked"`。如果你在 `switch result.Reason`，
  请加上 `ReasonLockContention` 分支——否则一次瞬时重试仍会被显示成"您的账号已被
  锁定"。这些 reason 现在是导出常量，原有取值的字符串没有变化。
- **用户锁定不能再被轮询延长。** 每次探测一个已耗尽的挑战都会再次调用 `lockUser`，
  把截止时间又推后一个 `LockoutDuration`——于是持有挑战 ID 的人可以把用户无限期锁在
  外面。现在锁定只在缺失时建立、永不刷新，由新增的 `Challenge.LockoutApplied` 记录。
  旧版本写入的挑战该字段默认为 `false`。
- **并发 Argon2 工作量有了上限。** *不同*挑战之间的验证此前完全没有上限，而每个在途
  验证都占着 Argon2 的内存成本（默认 64 MiB），100 个在途就是 6.4 GiB——对任何能用
  不同挑战 ID 调用 `Verify` 的人来说，这是一条内存耗尽路径。
  `MaxConcurrentVerifications`（默认 16）现在把它限住了。饱和时调用方会等待；
  等待期间 context 被取消的调用方得到可重试结果，而不是被消耗掉一次尝试。
- **`Create` 现在可能因熵不足而失败。** `generateChallengeID` 丢弃了兜底抽取的
  错误，而两次抽取都来自 `crypto/rand`、会一起失败——于是 token 为空，在一个注释
  声称已优雅处理该情况的函数里发生切片越界 panic。现在它返回包装了
  `ErrEntropyUnavailable` 的错误，`Create` 向上传播而不是签发一个可猜测的标识符。
  请处理这个你此前也许认为不可能出现的 `Create` 错误。
- **取消信号保留在错误链里。** `ErrLockUnavailable` 此前用 `%v` 折叠 context 错误，
  于是 `errors.Is(err, context.Canceled)` 为假，调用方会重试自己早已放弃的工作。
  现在改用 `%w` 包装。
- **后端错误不再被误判为过期。** 缓存未命中此前是靠匹配错误*文本*判断的，因为
  redis-kit v1.5.0 把未命中报成一个 `errors.Is` 看不穿的普通 `fmt.Errorf`。于是一个
  只是提到 "key not found" 的后端错误会变成 `ReasonExpired`——而它不消耗尝试次数，
  等于在基础设施故障期间白送一次探测。本次发布依赖 redis-kit v1.6.0，改用 `errors.Is`
  对 `cache.ErrKeyNotFound` 和 `redis.Nil` 判断，因此不健康的后端现在会如设计那样以
  `ReasonBackendUnavailable` 失败即关闭。
- **`DefaultConfig()` 每个字段都返回真实默认值。** `VerifyLock*`、
  `ActiveIndexPrefix` 和 `MaxConcurrentVerifications` 此前返回零值，尽管
  `NewManager` 内部会归一化它们。如果你通过复制 `DefaultConfig()` 再覆盖少数字段来
  构造 `Config`，现在拿到的是文档中的默认值而不是零。

## 安全说明

- **`Challenge.Destination` 在 Redis 中是明文存储的。** 活跃索引键特意做成不可逆
  摘要，使任何键里都不出现原始标识符，但挑战的*值*里仍然有手机号或邮箱——任何对该
  实例有读权限的人都能枚举收件目标。请对 Redis 做好访问控制，并把 `Expiry` 设短。
- **验证码从不明文存储**：用 Argon2 哈希，并以常量时间比较。
- **一次性使用**：成功返回之前挑战已被删除，删除失败绝不报告 OK。
- **失败即关闭**：任何 Redis 故障都返回 `ErrBackendUnavailable`，绝不退回本地的
  非原子路径。
- **尝试计数原子**：并发的错误验证码各自恰好让计数器加一，不会丢更新。

## 测试

```bash
go test ./...

# 带覆盖率
go test ./... -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out
```

## 依赖

- `github.com/redis/go-redis/v9` —— Redis 客户端
- `github.com/soulteary/secure-kit/v2` —— Argon2 哈希（`passwd` 子包）与安全随机数
- `github.com/alicebob/miniredis/v2` —— 仅测试使用

本仓库没有需要额外 import 的适配子包：与健康探针不同，这里的存储不是可选项 ——
没有 Redis 的 manager 根本不成立 —— 把它挪出根包对谁都没有好处。

## 许可证

Apache License 2.0 —— 详见 [LICENSE](LICENSE)。
