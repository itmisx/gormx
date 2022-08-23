package gormx

import (
	"context"
	"log"
	"os"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
)

type Config struct {
	Username       string `mapstructure:"username" yaml:"username"`
	Password       string `mapstructure:"password" yaml:"password"`
	Host           string `mapstructure:"host" yaml:"host"`
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

// New  new *gorm.DB
func New(cfg Config) (*gorm.DB, error) {
	if cfg.Charset == "" {
		cfg.Charset = "utf8mb4"
	}
	dsn := cfg.Username + ":" + cfg.Password +
		"@tcp(" + cfg.Host + ":" + cfg.Port + ")/" + cfg.Database + "?" +
		"charset=" + cfg.Charset + "&parseTime=True&loc=Local"
	mysqlConfig := mysql.Config{
		DSN:                       dsn,   // DSN data source name
		DefaultStringSize:         191,   // string 类型字段的默认长度
		DisableDatetimePrecision:  true,  // 禁用 datetime 精度，MySQL 5.6 之前的数据库不支持
		DontSupportRenameIndex:    true,  // 重命名索引时采用删除并新建的方式，MySQL 5.7 之前的数据库和 MariaDB 不支持重命名索引
		DontSupportRenameColumn:   true,  // 用 `change` 重命名列，MySQL 8 之前的数据库和 MariaDB 不支持重命名列
		SkipInitializeWithVersion: false, // 根据版本自动配置
	}

	newLogger := NewLogger(
		log.New(os.Stdout, "\r\n", log.LstdFlags), // io writer（日志输出的目标，前缀和日志包含的内容——译者注）
		glogger.Config{
			SlowThreshold:             time.Second,   // 慢 SQL 阈值
			LogLevel:                  glogger.Error, // 日志级别
			IgnoreRecordNotFoundError: true,          // 忽略ErrRecordNotFound（记录未找到）错误
			Colorful:                  false,         // 禁用彩色打印
		},
	)

	gormConfig := &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
		SkipDefaultTransaction:                   true, // 跳过默认事务
		Logger:                                   newLogger,
	}
	var db *gorm.DB
	var err error
	// 初始化mydql会话
	// 如果失败，每隔5s重试
	for {
		db, err = gorm.Open(mysql.New(mysqlConfig), gormConfig)
		if err != nil {
			log.Println(context.Background(), "mysql connection failed,retry...")
			time.Sleep(time.Second * 5)
		} else {
			break
		}
	}

	if cfg.Debug {
		db = db.Debug()
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}

	// 默认连接池为2
	if cfg.MaxIdleConns == 0 {
		cfg.MaxIdleConns = 2
	}

	// 最大空闲连接
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	// 连接最大空闲时间
	sqlDB.SetConnMaxIdleTime(time.Second * time.Duration(cfg.MaxIdleTime))
	// 最大连接数
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	// 连接最大可用时间
	sqlDB.SetConnMaxLifetime(time.Second * time.Duration(cfg.MaxLifetime))
	return db, nil
}
