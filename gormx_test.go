package gormx

import (
	"context"
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
