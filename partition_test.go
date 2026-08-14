package gormx

import (
	"context"
	"testing"
	"time"

	"github.com/dromara/carbon/v2"
)

// 分区单位不合法时应返回错误，而不是 panic 掉调用方进程
func TestPartitionInvalidUnit(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "part_demo",
		"id BIGINT NOT NULL AUTO_INCREMENT, created_at BIGINT NOT NULL, PRIMARY KEY(id, created_at)")

	p := NewPartition(db, testDatabase, "part_demo", PartitionUnitT(99), time.Hour)
	err := p.Start()
	if err == nil {
		t.Fatal("分区单位不合法时期望返回错误")
	}
}

// Start 应补齐未来两个周期的分区，并删除超出保留时长的旧分区
func TestPartitionStart(t *testing.T) {
	db := newTestDB(t)
	old := "p" + carbon.Now().SubDays(30).StartOfDay().ToShortDateString()
	cur := "p" + carbon.Now().StartOfDay().ToShortDateString()
	// 建一张已有「30 天前」和「今天」两个分区的表
	err := db.Exec(`CREATE TABLE part_demo (
		id BIGINT NOT NULL AUTO_INCREMENT,
		created_at BIGINT NOT NULL,
		PRIMARY KEY (id, created_at)
	) PARTITION BY RANGE (created_at) (
		PARTITION ` + old + ` VALUES LESS THAN (UNIX_TIMESTAMP('` +
		carbon.Now().SubDays(30).StartOfDay().ToDateString() + `')),
		PARTITION ` + cur + ` VALUES LESS THAN (UNIX_TIMESTAMP('` +
		carbon.Now().StartOfDay().ToDateString() + `'))
	)`).Error
	if err != nil {
		t.Fatalf("建表失败：%v", err)
	}

	// 只保留 7 天，30 天前那个分区应被删除
	p := NewPartition(db, testDatabase, "part_demo", PartitionUnitDay, time.Hour*24*7)
	if err := p.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if err := p.dropExpiredPartitions(context.Background()); err != nil {
		t.Fatalf("删除过期分区失败：%v", err)
	}

	parts, err := p.list(context.Background())
	if err != nil {
		t.Fatalf("获取分区列表失败：%v", err)
	}
	has := func(name string) bool {
		for _, v := range parts {
			if v == name {
				return true
			}
		}
		return false
	}
	// 过期分区已删除
	if has(old) {
		t.Errorf("过期分区 %s 应被删除，实际分区：%v", old, parts)
	}
	// 未来两个周期的分区已补齐
	for _, d := range []int{1, 2} {
		want := "p" + carbon.Now().AddDays(d).StartOfDay().ToShortDateString()
		if !has(want) {
			t.Errorf("未补齐分区 %s，实际分区：%v", want, parts)
		}
	}
}
