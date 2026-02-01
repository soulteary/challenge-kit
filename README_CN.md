# challenge-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/challenge-kit.svg)](https://pkg.go.dev/github.com/soulteary/challenge-kit)
[![Go Report Card](https://goreportcard.com/badge/github.com/soulteary/challenge-kit)](https://goreportcard.com/report/github.com/soulteary/challenge-kit)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![codecov](https://codecov.io/gh/soulteary/challenge-kit/graph/badge.svg)](https://codecov.io/gh/soulteary/challenge-kit)

[English](README.md)

一个用于管理 OTP（一次性密码）挑战的 Go 库，支持挑战生命周期管理、验证码生成、验证和用户锁定机制。

## 功能特性

- **挑战生命周期管理**：创建、验证和撤销挑战
- **OTP 验证码生成**：安全的随机数字验证码生成
- **验证码验证**：基于 Argon2 的安全验证码哈希和验证
- **尝试次数跟踪**：自动跟踪验证尝试次数
- **用户锁定**：在达到最大失败尝试次数后自动锁定用户
- **Redis 存储**：内置 Redis 支持，用于挑战和锁定状态存储
- **可配置**：灵活的配置选项，包括过期时间、尝试次数和锁定时长
- **线程安全**：支持并发访问

## 安装

```bash
go get github.com/soulteary/challenge-kit
```

## 快速开始

### 基本用法

```go
package main

import (
    "context"
    "time"
    
    "github.com/redis/go-redis/v9"
    "github.com/soulteary/challenge-kit"
)

func main() {
    // 创建 Redis 客户端
    redisClient := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })
    defer redisClient.Close()

    // 使用默认配置创建挑战管理器
    config := challenge.DefaultConfig()
    manager := challenge.NewManager(redisClient, config)

    ctx := context.Background()

    // 创建挑战
    req := challenge.CreateRequest{
        UserID:      "user123",
        Channel:     challenge.ChannelEmail,
        Destination: "user@example.com",
        Purpose:     "login",
        ClientIP:    "127.0.0.1",
    }

    ch, code, err := manager.Create(ctx, req)
    if err != nil {
        panic(err)
    }

    // 向用户发送验证码（通过短信/邮件）
    // ...

    // 验证验证码
    result, err := manager.Verify(ctx, ch.ID, code, req.ClientIP)
    if err != nil {
        // 处理错误
    }

    if result.OK {
        // 挑战验证成功
        // 继续认证流程
    } else {
        // 处理验证失败
        // result.Reason 包含："expired", "invalid", "locked", "user_locked"
        // result.RemainingAttempts 包含剩余尝试次数（如果适用）
    }
}
```

### 自定义配置

```go
config := challenge.Config{
    Expiry:           5 * time.Minute,  // 挑战 TTL
    MaxAttempts:      5,                // 最大验证尝试次数
    LockoutDuration:  10 * time.Minute, // 用户锁定时长
    CodeLength:       6,                 // OTP 验证码长度（1-10）
    ChallengeKeyPrefix: "otp:ch:",       // Redis 挑战键前缀
    LockKeyPrefix:     "otp:lock:",      // Redis 锁定键前缀
}

manager := challenge.NewManager(redisClient, config)
```

## API 参考

### 类型

#### Challenge

```go
type Challenge struct {
    ID          string
    UserID      string
    Channel     Channel  // "sms" | "email"
    Destination string
    CodeHash    string
    Purpose     string
    ExpiresAt   time.Time
    Attempts    int
    MaxAttempts int
    CreatedIP   string
    CreatedAt   time.Time
}
```

#### CreateRequest

```go
type CreateRequest struct {
    UserID      string
    Channel     Channel
    Destination string
    Purpose     string
    ClientIP    string
}
```

#### VerifyResult

```go
type VerifyResult struct {
    OK                bool
    Challenge         *Challenge
    Reason            string  // "expired", "invalid", "locked", "user_locked"
    RemainingAttempts *int
}
```

### 方法

#### Create

创建一个新挑战并存储到 Redis。返回挑战、明文验证码（用于发送）和任何错误。

```go
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Challenge, string, error)
```

#### Verify

验证验证码是否匹配挑战。返回包含验证状态的 VerifyResult。

```go
func (m *Manager) Verify(ctx context.Context, challengeID, code, clientIP string) (*VerifyResult, error)
```

#### Revoke

撤销挑战，从存储中删除。

```go
func (m *Manager) Revoke(ctx context.Context, challengeID string) error
```

#### IsUserLocked

检查用户是否因失败尝试次数过多而被锁定。

```go
func (m *Manager) IsUserLocked(ctx context.Context, userID string) bool
```

#### Get

根据 ID 检索挑战。

```go
func (m *Manager) Get(ctx context.Context, challengeID string) (*Challenge, error)
```

### 辅助函数

#### GenerateCode

生成指定长度的随机数字验证码。

```go
func GenerateCode(length int) (string, error)
```

#### ValidateCodeFormat

验证验证码是否符合预期格式（数字，正确长度）。

```go
func ValidateCodeFormat(code string, length int) bool
```

## 安全特性

- **安全验证码生成**：使用 `crypto/rand` 进行密码学安全的随机数生成
- **Argon2 哈希**：验证码在存储前使用 Argon2 进行哈希（从不以明文存储）
- **恒定时间验证**：验证码验证使用恒定时间比较以防止时序攻击
- **一次性使用**：挑战在成功验证后自动删除
- **自动过期**：挑战基于 TTL 过期（由 Redis 处理）
- **用户锁定**：在达到最大失败尝试次数后自动锁定用户

## 依赖

- `github.com/redis/go-redis/v9` - Redis 客户端
- `github.com/soulteary/redis-kit` - Redis 缓存接口
- `github.com/soulteary/secure-kit` - 安全哈希和随机生成

## 许可证

Apache License 2.0
