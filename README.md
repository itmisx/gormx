# gormx

重新实现了gorm的日志记录器，采用github.com/itmisx/logger

#### 安装

```bash
go get -u -v github.com/itmisx/gormx
```

#### 配置

```go
type Config struct {
	Username     string   `mapstructure:"username" yaml:"username"`             // 用户名
	Password     string   `mapstructure:"password" yaml:"password"`             // 密码
	Addrs        []string `mapstructure:"addrs" yaml:"addrs"`                   // 连接地址(host:port),多个以逗号分隔,Addrs[0]为master，其余为slave
	Database     string   `mapstructure:"database" yaml:"database"`             // 要连接的数据库
	Charset      string   `mapstructure:"charset" yaml:"charset"`               // 字符集
	Debug        bool     `mapstructure:"debug" yaml:"debug"`                   // 是否开启调试模式
	MaxOpenConns int      `mapstructure:"max_open_conns" yaml:"max_open_conns"` // 设置数据库的最大打开连接数
	MaxLifetime  int      `mapstructure:"max_lifetime" yaml:"max_lifetime"`     // 设置连接可以重用的最长时间(单位：秒)
	MaxIdleConns int      `mapstructure:"max_idle_conns" yaml:"max_idle_conns"` // 设置空闲连接池中的最大连接数
	MaxIdleTime  int      `mapstructure:"max_idle_time" yaml:"max_idle_time"`   // 设置空闲连接池中的最大连接数
}

#### 使用

```go
db, _ := gormx.New(cfg)
// 通过WithContext传入追踪参数
db.Where(……).WithContext(spanCtx).Find(&users)
```
