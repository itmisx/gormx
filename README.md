<div align="center">

# gormx

**GORM 增强工具集** — 读写分离 · 自动分区 · 在线表结构变更 · 数据库版本管理

[![Go Reference](https://pkg.go.dev/badge/github.com/itmisx/gormx.svg)](https://pkg.go.dev/github.com/itmisx/gormx)
[![Go Version](https://img.shields.io/github/go-mod/go-version/itmisx/gormx)](https://github.com/itmisx/gormx)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

</div>

---

gormx 不替代 GORM，而是在它之上补齐生产环境里几件绕不开的事：从库分流、大表按时间滚动、线上改表不锁库、多环境结构对齐。全部围绕 **MySQL** 设计。

## 该用哪个

| 你遇到的问题 | 用这个 | 跳转 |
| :--- | :--- | :--- |
| 读流量压垮主库，想让从库分担查询 | 读写分离 | [→](#读写分离) |
| 日志/流水/快照表越来越大，查询变慢、清理老数据一删就锁表 | 自动分区 | [→](#自动分区) |
| 要给线上大表加字段、改索引、改主键，怕锁表阻塞业务 | 在线表结构变更 | [→](#在线表结构变更) |
| 多套环境的库结构不一致，想让程序启动时自动升级到同一版本 | 版本管理 | [→](#版本管理) |
| 想统一 SQL 日志格式、把慢查询接到告警系统 | 日志与慢查询 | [→](#日志与慢查询) |

## 安装

```bash
go get -u github.com/itmisx/gormx
```

## 快速开始

```go
db, err := gormx.New(gormx.Config{
    Username: "root",
    Password: "123456",
    Addrs:    []string{"127.0.0.1:3306"}, // Addrs[0] 为主库
    Database: "app",
})
if err != nil {
    panic(err)
}

// 与原生 GORM 完全一致，WithContext 传入的 ctx 会带上链路追踪信息
db.WithContext(ctx).Where("age > ?", 18).Find(&users)
```

`gormx.New` 返回的就是标准的 `*gorm.DB`，原有代码不需要任何改动。

## 配置

```go
type Config struct {
    Username       string
    Password       string
    Addrs          []string // 连接地址(host:port)，Addrs[0] 为主库，其余为从库
    Database       string
    Charset        string
    Debug          bool
    MaxOpenConns   int // 最大连接数
    MaxIdleConns   int // 空闲连接数
    MaxLifetime    int // 连接最长存活时间，单位：秒
    MaxIdleTime    int // 空闲连接最长保留时间，单位：秒
    SlowSqlHandler func(sql string, elapsed int64)
}
```

| 字段 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `Charset` | `utf8mb4` | 字符集 |
| `Debug` | `false` | 开启后打印全部 SQL |
| `MaxIdleConns` | `2` | 空闲连接池大小 |
| `MaxIdleTime` | `60` | 空闲多久回收，低峰期不长期占用服务端会话 |
| `MaxLifetime` | `300` | 连接最长存活时间 |
| `MaxOpenConns` | `0`（不限制） | 最大连接数 |
| `SlowSqlHandler` | `nil` | 慢 SQL 回调，见[日志与慢查询](#日志与慢查询) |

> [!IMPORTANT]
> `MaxLifetime` 默认 300 秒是**有意取小**的。设为 0 表示连接永久复用，但服务端到点会主动断开（MySQL `wait_timeout` 默认 28800 秒），客户端再复用就会报 `invalid connection`——典型表现是低峰期后第一批请求偶发失败。链路中间若有 SLB、ProxySQL、K8s conntrack，它们的空闲超时通常只有 5~15 分钟，300 秒能一并兜住。
>
> `MaxOpenConns` 保持 0 意味着**不限制**，连接数可以一直涨到吃满 MySQL 的 `max_connections`。生产环境建议按部署规模显式配置。

---

## 读写分离

### 适用场景

读多写少、已经有从库、且业务能接受主从延迟的场景。

### 不适用

- **写后立即读**：写主库、读从库，主从延迟内读到旧值。这类逻辑需要显式走主库
- 只有单实例数据库——配置一个地址即可，不会引入 dbresolver

### 用法

在 `Addrs` 里多配几个地址就行，`Addrs[0]` 是主库，其余作为从库，按随机策略负载均衡：

```go
db, _ := gormx.New(gormx.Config{
    Addrs: []string{
        "10.0.0.1:3306", // 主库：所有写操作
        "10.0.0.2:3306", // 从库
        "10.0.0.3:3306", // 从库
    },
    // ...
})
```

需要强制读主库时：

```go
import "gorm.io/plugin/dbresolver"

db.Clauses(dbresolver.Write).Find(&user)
```

> [!NOTE]
> 事务内的所有语句都会走主库，`SELECT ... FOR UPDATE` 也会自动路由到主库。

---

## 自动分区

按**时间字段**对表做 RANGE 分区，自动创建未来的分区、自动删除过期分区。

### 适用场景

- 日志、流水、监控快照这类**只追加、按时间查询、过期即可丢弃**的表
- 需要周期性清理历史数据，但 `DELETE` 会长时间锁表、且不释放磁盘空间

分区的价值在于：删除历史数据变成 `DROP PARTITION`，是**秒级的元数据操作**，且立刻释放磁盘。

### 不适用

- 需要按非时间维度做主键查询的表（分区键必须包含在每个唯一索引里，会强制改主键）
- 数据量不大、没有清理需求的表——分区只会增加复杂度

### 建表要求

分区键必须包含在主键中，且时间字段建议存 Unix 时间戳：

```sql
CREATE TABLE IF NOT EXISTS device_status (
  id         BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
  created_at BIGINT NOT NULL COMMENT '创建时间',
  PRIMARY KEY (id, created_at)          -- 分区键 created_at 必须在主键里
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
PARTITION BY RANGE (created_at) (
  PARTITION p20250101 VALUES LESS THAN (UNIX_TIMESTAMP('2025-01-01')),
  PARTITION p20250201 VALUES LESS THAN (UNIX_TIMESTAMP('2025-02-01'))
);
```

> [!WARNING]
> **不要加 `PARTITION pmax VALUES LESS THAN MAXVALUE`。** 一旦存在 MAXVALUE 兜底分区，后续 `ADD PARTITION` 会直接报错，自动分区就失效了。

### 用法

```go
p := gormx.NewPartition(
    db,
    "app",                    // 数据库名
    "device_status",          // 表名
    gormx.PartitionUnitMonth, // 分区单位：Day / Month / Year
    time.Hour*24*180,         // 数据保留时长
)
if err := p.Start(); err != nil {
    // 分区单位不合法、或初始化建分区失败
}
```

`Start()` 会立即补齐未来两个周期的分区，然后启动后台协程周期性检查——补建新分区、`DROP` 掉超出保留时长的旧分区。

| 参数 | 说明 |
| :--- | :--- |
| `partitionUnit` | `PartitionUnitDay` / `PartitionUnitMonth` / `PartitionUnitYear` |
| `retentionDuration` | `time.Duration` 类型。传 `0` 表示只创建、不删除 |

检查周期由 `gormx.DefaultCronDuration` 控制，默认 1 小时，最小 10 秒：

```go
gormx.DefaultCronDuration = time.Minute * 30 // 需在 Start() 之前设置
```

> [!NOTE]
> 分区名形如 `p20250101`，由分区单位对应的日期生成。过期判断是按分区名里的日期数字比较的，因此**不要手工创建不符合该命名规则的分区**。

---

## 在线表结构变更

给线上表执行 `ALTER`，尽量不阻塞业务读写。

### 两条路径

调用 `Start()` 后会先尝试 **MySQL 原生 Online DDL**（自动追加 `ALGORITHM=INPLACE, LOCK=NONE`）：

- **成功** → 变更直接完成，没有任何额外开销
- **失败且 MySQL 明确报出「不支持该算法」（错误码 1845/1846）** → 回退到复制表迁移
- **其他错误**（语法错误、列不存在、权限不足等）→ 直接返回错误，**不会**回退

复制表迁移的流程：建影子表 → 在影子表上执行 ALTER → 业务写入双写到影子表 → 后台分批回填历史数据 → `RENAME` 原子切换。

### 适用场景

- 大表加列、改列类型、加/删索引、改主键、加分区
- 变更期间业务不能停

### 不适用

> [!CAUTION]
> **表上有外键时，复制表迁移无法使用。** 外键约束名在 MySQL 中是库级唯一的，复制表结构时必然重名（`Error 1826`）。即使绕开重名也还有两个无解的问题：`RENAME` 会把子表的外键引用带到备份表上；级联删除由存储引擎执行，不经过 GORM 的钩子，双写拦不到。gh-ost 等同类工具同样不支持外键。
>
> 这条只影响复制表路径，能走原生 Online DDL 的变更不受影响。

其他限制：

- **表必须有整型 `id` 列**（回填游标依赖它）。不满足会在建影子表之前就返回错误，不留任何中间产物
- **绕过 GORM 的写入不会被双写捕获**：触发器、其他服务/语言直连写库、项目里的裸 SQL
- 迁移期间读操作仍然只走原表，没有额外开销

### 用法

待迁移的 model 匿名嵌入 `gormx.Migration`：

```go
type DeviceStatus struct {
    ID        int    `gorm:"column:id;primaryKey;autoIncrement"`
    Name      string `gorm:"column:name;type:varchar(20)"`
    CreatedAt int64  `gorm:"column:created_at;autoCreateTime"`
    gormx.Migration `gorm:"-"` // 必须带 gorm:"-"
}
```

启动迁移：

```go
m := gormx.NewMigration(
    db,
    "device_status",
    nil, nil, nil, // 可选的 AfterCreate / AfterUpdate / AfterDelete 回调
    `alter table device_status
     drop primary key,
     add primary key(id, created_at)
     PARTITION BY RANGE (created_at) (
       PARTITION p20250101 VALUES LESS THAN (UNIX_TIMESTAMP('2025-01-01')),
       PARTITION p20250201 VALUES LESS THAN (UNIX_TIMESTAMP('2025-02-01'))
     )`,
)
if err := m.Start(); err != nil {
    // 变更失败，原表未被修改
}
```

后三个参数是业务自己的钩子。

> [!WARNING]
> 双写依赖嵌入的 `AfterCreate` / `AfterUpdate` / `AfterDelete` 方法。**model 自身若定义了同名方法，会把嵌入的方法遮蔽掉，双写直接失效。** 遇到这种情况把原方法改名，再从这三个参数传进来：

```go
gormx.NewMigration(db, "device_status",
    func(tx *gorm.DB) error {
        // 原本写在 DeviceStatus.AfterCreate 里的逻辑挪到这里
        return nil
    },
    nil, nil,
    alterSQL,
)
```

双写成功后，gormx 会接着调用这些回调；回调返回错误会中断本次写入。

### 注意事项

- **同一张表同时只允许一个迁移**。多实例部署时由 MySQL 用户级锁（`GET_LOCK`）保证，未抢到锁的实例会跳过本轮
- **切换后旧表会保留为 `<表名>_old_<时间戳>`**，确认无误后需手工删除
- 迁移进度记录在 `gorm_migration_log` 表，可查询 `total_records` / `completed_records` 观察回填进度
- 回填速率固定为 200 行/100ms（约 2000 行/秒），亿级大表需评估耗时
- 多实例场景下，其他实例发起的迁移最多延迟 `gormx.ShadowTableRefreshInterval`（默认 3 秒）才会开始双写。这段延迟是安全的——期间的写入会被历史数据回填带到影子表

---

## 版本管理

让程序启动时把数据库结构自动升级到当前代码期望的版本，多环境不再手工对齐。

### 适用场景

- 私有化部署、多环境（开发/测试/生产）需要保证库结构一致
- 升级步骤里既有 DDL 也有数据订正逻辑，需要用 Go 代码表达

### 用法

版本号从方法名里的数字解析，按升序依次执行，执行记录写入 `version_log` 表：

```go
// 首次安装：库里没有任何版本记录时执行
func install() {
    db.AutoMigrate(&User{}, &Order{})
}

type Upgrade struct{}

func (Upgrade) V1() error {
    return db.Exec("alter table users add column age int").Error
}

func (Upgrade) V2() error {
    // 只需执行一次的语句用 MigrateOnce 包起来
    return gormx.MigrateOnce(db, "backfill_age", func() error {
        return db.Exec("update users set age = 18 where age is null").Error
    })
}

vc := gormx.NewVersionController(db, Upgrade{}, install)
if err := vc.Upgrade(); err != nil {
    panic(err)
}
```

| 概念 | 说明 |
| :--- | :--- |
| 版本号 | 取方法名里的第一段数字，`V1` → 1、`V20250101` → 20250101 |
| `install` | 仅在库中没有任何版本记录时执行，用于首次建表 |
| `MigrateOnce` | 以「版本号 + 名称」为唯一键，保证同一段逻辑只执行一次 |

> [!IMPORTANT]
> `InstallFunc` 必须建出**当前版本的完整结构**。库中没有版本记录时，gormx 会执行它并把版本直接置为最高版本，中间各版本的升级函数不会执行。
>
> 另外，与当前版本号相同的那个升级函数**每次 `Upgrade()` 都会重新执行**——非幂等语句务必用 `MigrateOnce` 包起来。`MigrateOnce` 保证只执行一次，失败时不会留下执行记录，下次启动可以重试。

---

## 日志与慢查询

`gormx.New` 会自动装配定制的日志器，SQL 日志通过 [logx](https://github.com/itmisx/logx) 输出并带上链路追踪信息。慢查询阈值为 1 秒。

把慢 SQL 接到自己的告警/统计系统：

```go
db, _ := gormx.New(gormx.Config{
    // ...
    SlowSqlHandler: func(sql string, elapsed int64) {
        // elapsed 单位：毫秒
        metrics.SlowQuery.Inc()
        alert.Send(fmt.Sprintf("慢查询 %dms: %s", elapsed, sql))
    },
})
```

开启 `Debug: true` 会打印全部 SQL，仅建议在开发环境使用。

---

## License

[MIT](./LICENSE)
