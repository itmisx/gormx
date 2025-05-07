package gormx

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/itmisx/logx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	logger "gorm.io/gorm/logger"
	"gorm.io/plugin/dbresolver"
)

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

// New  new *gorm.DB
func New(cfg Config) (db *gorm.DB, err error) {
	var (
		replicas []gorm.Dialector
	)
	if cfg.Charset == "" {
		cfg.Charset = "utf8mb4"
	}

	// 自定义日志
	myLogger := NewLogger(
		log.New(os.Stdout, "\r\n", log.LstdFlags), // io writer（日志输出的目标，前缀和日志包含的内容——译者注）
		logger.Config{
			SlowThreshold:             time.Second,  // 慢 SQL 阈值
			LogLevel:                  logger.Error, // 日志级别
			IgnoreRecordNotFoundError: true,         // 忽略ErrRecordNotFound（记录未找到）错误
			Colorful:                  true,         // 彩色打印
		},
	)

	// 默认连接池为2
	if cfg.MaxIdleConns == 0 {
		cfg.MaxIdleConns = 2
	}

	for index, host := range cfg.Addrs {
		dsn := cfg.Username + ":" + cfg.Password + "@tcp(" + host + ")/" + cfg.Database + "?" + "charset=" + cfg.Charset + "&parseTime=True&loc=Local&timeout=5s"
		// master
		if index == 0 {
			for {
				db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: myLogger})
				if err != nil {
					logx.Error(context.Background(), "mysql connection failed,retry...")
				} else {
					if cfg.Debug {
						db = db.Debug()
					} else {
						db = db.Session(&gorm.Session{})
					}
					sqlDB, err := db.DB()
					if err != nil {
						return nil, err
					}
					sqlDB.SetConnMaxIdleTime(time.Second * time.Duration(cfg.MaxIdleTime))
					sqlDB.SetConnMaxLifetime(time.Second * time.Duration(cfg.MaxLifetime))
					sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
					sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
					break
				}
			}
		} else { // replicas
			replicas = append(replicas, mysql.Open(dsn))
		}
	}

	if len(replicas) > 0 {
		for {
			err := db.Use(
				dbresolver.Register(
					dbresolver.Config{
						Replicas:          replicas,                  // replicas
						Policy:            dbresolver.RandomPolicy{}, // 负载均衡策略
						TraceResolverMode: true,                      // 打印master/replicas mode 日志
					}).
					SetConnMaxIdleTime(time.Second * time.Duration(cfg.MaxIdleTime)).
					SetConnMaxLifetime(time.Second * time.Duration(cfg.MaxLifetime)).
					SetMaxIdleConns(cfg.MaxIdleConns).
					SetMaxOpenConns(cfg.MaxOpenConns),
			)
			if err != nil {
				logx.Error(context.Background(), "replicas connection failed,retry...", logx.Err(err))
			} else {
				break
			}
		}
	}
	return db, nil
}
