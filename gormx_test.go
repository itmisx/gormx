package gormx

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/itmisx/logx"
)

func TestGormx(t *testing.T) {
	// 初始化日志
	logx.Init(
		logx.Config{
			Debug:  true,
			Output: "console",
		},
	)
	// 实例化gorm
	db, err := New(Config{
		Username: "root",
		Password: "123456",
		Addrs:    []string{"127.0.0.1:13306", "127.0.0.1:13306"},
		Database: "device",
		Debug:    true,
	})
	if err != nil {
		panic(err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	// 查询
	var record map[string]interface{}
	db.WithContext(ctx).Table("device").Take(&record)
	// 更新
	db.WithContext(ctx).Table("device").Where("id = ?", record["id"]).Update("updated_at", time.Now().Unix())
}

// 迁移测试
type migrationTest struct {
	ID        int    `json:"id" gorm:"column:id;type:int(8);primaryKey;autoIncrement"`
	Name      string `json:"name" gorm:"column:name;type:varchar(20)"`
	CreatedAt int64  `json:"created_at" gorm:"column:created_at;type:bigint;autoCreateTime;default:0"`
	Migration `gorm:"-"`
}

func (migrationTest) TableName() string {
	return "migration_test"
}
func TestMigrate(t *testing.T) {
	// 初始化日志
	logx.Init(
		logx.Config{
			Debug:  true,
			Output: "console",
		},
	)
	// 实例化gorm
	db, err := New(Config{
		Username: "root",
		Password: "123456",
		Addrs:    []string{"127.0.0.1:13306", "127.0.0.1:13306"},
		Database: "migration",
		Debug:    true,
	})
	if err != nil {
		panic(err.Error())
	}

	db.Migrator().AutoMigrate(&migrationTest{})

	// 循环插入数据
	go func() {
		for {
			db.Model(&migrationTest{}).Create([]migrationTest{
				{Name: strconv.Itoa(int(time.Now().UnixNano()))},
				{Name: strconv.Itoa(int(time.Now().UnixNano()))},
			})
			time.Sleep(time.Millisecond * 50)
		}
	}()

	// 开始迁移
	time.Sleep(time.Second * 2)
	migration := NewMigration(
		db,
		"migration_test",
		nil, nil, nil,
		"alter table migration_test modify column c1 varchar(10)",
	)
	migration.Start()

	// 阻塞
	<-make(chan struct{})
}

func TestMigratePartition(t *testing.T) {
	// 初始化日志
	logx.Init(
		logx.Config{
			Debug:  true,
			Output: "console",
		},
	)
	// 实例化gorm
	db, err := New(Config{
		Username: "root",
		Password: "123456",
		Addrs:    []string{"127.0.0.1:13306", "127.0.0.1:13306"},
		Database: "migration",
		Debug:    true,
	})
	if err != nil {
		panic(err.Error())
	}

	db.Migrator().AutoMigrate(&migrationTest{})
	// 开始迁移
	time.Sleep(time.Second * 2)
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

	// 阻塞
	<-make(chan struct{})
}
