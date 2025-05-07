# gormx

重新实现了gorm的日志记录器，采用github.com/itmisx/logger

#### 安装

```bash
go get -u -v github.com/itmisx/gormx
```

#### 配置

```go
type Config struct {
	Username       string `mapstructure:"username" yaml:"username"`
	Password       string `mapstructure:"password" yaml:"password"`
	Addrs          string `mapstructure:"addrs" yaml:"addrs"`
	Port           string `mapstructure:"port" yaml:"port"`
	Database       string `mapstructure:"database" yaml:"database"`
	Charset        string `mapstructure:"charset" yaml:"charset"`
	ConnectTimeout int    `mapstructure:"connect_timeout" yaml:"connect_timeout"`
	Debug          bool   `mapstructure:"debug" yaml:"debug"`
	MaxOpenConns   int    `mapstructure:"max_open_conns" yaml:"max_open_conns"` // 设置数据库的最大打开连接数
	MaxLifetime    int    `mapstructure:"max_lifetime" yaml:"max_lifetime"`     // 设置连接可以重用的最长时间(单位：秒)
	MaxIdleConns   int    `mapstructure:"max_idle_conns" yaml:"max_idle_conns"` // 设置空闲连接池中的最大连接数
	MaxIdleTime    int    `mapstructure:"max_idle_time" yaml:"max_idle_time"`   // 设置空闲连接池中的最大连接数
}

#### 使用

```go
db, _ := gormx.New(cfg)
// 通过WithContext传入追踪参数
db.Where(……).WithContext(spanCtx).Find(&users)
```
