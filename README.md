# gormx

- 🚀 重新实现了 gorm 的日志记录器，采用 github.com/itmisx/logger
- 🚀 实现了读写分离
- 🚀 实现了数据库的自动分区管理
- 🚀 实现了数据库结构在线变更(online ddl)

### 一、安装

```bash
go get -u -v github.com/itmisx/gormx
```

### 二、配置

```go
type Config struct {
  Username     string   `mapstructure:"username" yaml:"username"`             // 用户名
  Password     string   `mapstructure:"password" yaml:"password"`             // 密码
  Addrs        []string `mapstructure:"addrs" yaml:"addrs"`                   // 连接地址(host:port),Addrs[0]为. master，其余为slave
  Database     string   `mapstructure:"database" yaml:"database"`             // 要连接的数据库
  Charset      string   `mapstructure:"charset" yaml:"charset"`               // 字符集
  Debug        bool     `mapstructure:"debug" yaml:"debug"`                   // 是否开启调试模式
  MaxOpenConns int      `mapstructure:"max_open_conns" yaml:"max_open_conns"` // 设置数据库的最大打开连接数
  MaxLifetime  int      `mapstructure:"max_lifetime" yaml:"max_lifetime"`     // 设置连接可以重用的最长时间(单位：秒)
  MaxIdleConns int      `mapstructure:"max_idle_conns" yaml:"max_idle_conns"` // 设置空闲连接池中的最大连接数
  MaxIdleTime  int      `mapstructure:"max_idle_time" yaml:"max_idle_time"`   // 设置空闲连接池中的最大连接数
}
```

### 三、使用

```go
db, _ := gormx.New(cfg)
// 通过WithContext传入追踪参数
db.Where(……).WithContext(spanCtx).Find(&users)
```

### 四、读写分离

> 通过配置参数 addrs 配置

### 五、分区

##### 示例

```go
partition := gormx.NewPartition(
    db,
    database,
    "device_status_snapshot",
    gormx.PartitionUnitMonth,
    6,
)
partition.Start()
```

##### 参数说明

- db，gorm 连接
- database，操作的数据库
- table，操作的表
- partitionUnit，分区单位。支持按天、按月、按年分区
- retentionMonths，分区数据保留的时长，单位月

##### 自动分区说明

- 调用 partition.Start() 启动自动分区
- 并自动 drop 过期的分区

### 六、Online DDL

- 表需要使用 id(int)作为主键
- 自动判断是否支持 Online ddl，如果不支持则自动创建新表，并通过 gorm 的钩子进行双写，并自动进行工作表切换。旧表需要手动删除
- 示例
```go
migration := NewMigration(
  db,
  "migration_test",
  nil, nil, nil,
  `alter table migration_test
  drop primary key,
  add primary key(id,created_at)
  PARTITION BY RANGE (created_at) (
      PARTITION p20241101 VALUES LESS THAN (UNIX_TIMESTAMP('2024-11-01')),
      PARTITION p20241201 VALUES LESS THAN (UNIX_TIMESTAMP('2024-12-01')),
      PARTITION p20250101 VALUES LESS THAN (UNIX_TIMESTAMP('2025-01-01')),
      PARTITION p20250201 VALUES LESS THAN (UNIX_TIMESTAMP('2025-02-01')),
      PARTITION p20250301 VALUES LESS THAN (UNIX_TIMESTAMP('2025-03-01')),
      PARTITION p20250401 VALUES LESS THAN (UNIX_TIMESTAMP('2025-04-01')),
      PARTITION p20250501 VALUES LESS THAN (UNIX_TIMESTAMP('2025-05-01')),
      PARTITION p20250601 VALUES LESS THAN (UNIX_TIMESTAMP('2025-06-01')),
      PARTITION p20250701 VALUES LESS THAN (UNIX_TIMESTAMP('2025-07-01'))
  )`,
)
migration.Start()
```
