package gormx

// 按时间范围分区，适配如下分区定义的
// CREATE TABLE IF NOT EXISTS table (
//   id bigint NOT NULL AUTO_INCREMENT COMMENT '主键',
//   created_at BIGINT NOT NULL COMMENT '创建时间',
//   PRIMARY KEY (id, created_at)
// )ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
// PARTITION BY RANGE (created_at) (
//   PARTITION p20241101 VALUES LESS THAN (UNIX_TIMESTAMP('2024-11-01')),
//   PARTITION p20241201 VALUES LESS THAN (UNIX_TIMESTAMP('2024-12-01')),
//   PARTITION p20250101 VALUES LESS THAN (UNIX_TIMESTAMP('2025-01-01')),
//   PARTITION p20250201 VALUES LESS THAN (UNIX_TIMESTAMP('2025-02-01')),
//   PARTITION p20250301 VALUES LESS THAN (UNIX_TIMESTAMP('2025-03-01')),
//   PARTITION p20250401 VALUES LESS THAN (UNIX_TIMESTAMP('2025-04-01')),
//   PARTITION p20250501 VALUES LESS THAN (UNIX_TIMESTAMP('2025-05-01')),
//   PARTITION p20250601 VALUES LESS THAN (UNIX_TIMESTAMP('2025-06-01'))
//   PARTITION pmax VALUES LESS THAN MAXVALUE  // !!! 如果需要动态新增分区，该语句需要去掉，否则会报错，这是个兜底，不能再加分区
// );

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/dromara/carbon/v2"
	"github.com/itmisx/logx"
	"github.com/samber/lo"
	"gorm.io/gorm"
)

type partition struct {
	db       *gorm.DB
	database string // 数据库名
	table    string // 表名
}

func NewPartition(
	db *gorm.DB,
	database string,
	table string,
) *partition {
	return &partition{
		db:       db,
		database: database,
		table:    table,
	}
}

// List 获取所有分区
func (p *partition) List(ctx context.Context) (partitions []string, err error) {
	// 获取所有分区
	err = p.db.Table("information_schema.PARTITIONS").
		Where("TABLE_SCHEMA = ?", p.database).
		Where("TABLE_NAME = ?", p.table).
		Where("PARTITION_NAME IS NOT NULL").
		Pluck("PARTITION_NAME", &partitions).
		Error
	return partitions, err
}

// ExistsCurrentMonth 是否存在当月分区
func (p *partition) ExistsCurrentMonthPartition(ctx context.Context) (bool, error) {
	partitions, err := p.List(ctx)
	if err != nil {
		return false, err
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddMonths(1).StartOfMonth().ToDateString(), "-", "")
	return lo.Contains(partitions, partitionName), nil
}

// ExistsNextMonth 是否存在下个月分区
func (p *partition) ExistsNextMonthPartition(ctx context.Context) (bool, error) {
	partitions, err := p.List(ctx)
	if err != nil {
		return false, err
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddMonths(2).StartOfMonth().ToDateString(), "-", "")
	return lo.Contains(partitions, partitionName), nil
}

// AddCurrentMonthPartition 新增当月分区
func (p *partition) AddCurrentMonthPartition(ctx context.Context) error {
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddMonths(1).StartOfMonth().ToDateString(), "-", "")
	partitionRange := carbon.Now().AddMonths(1).StartOfMonth().ToDateString()
	createPartitionSQL := fmt.Sprintf(
		"ALTER TABLE %s ADD PARTITION(PARTITION %s VALUES LESS THAN (UNIX_TIMESTAMP('%s')))",
		p.table,
		partitionName,
		partitionRange,
	)
	return p.db.Exec(createPartitionSQL).Error
}

// AddNextMonthPartition 新增下月分区
func (p *partition) AddNextMonthPartition(ctx context.Context) error {
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddMonths(2).StartOfMonth().ToDateString(), "-", "")
	partitionRange := carbon.Now().AddMonths(2).StartOfMonth().ToDateString()
	sql := fmt.Sprintf(
		"ALTER TABLE %s ADD PARTITION(PARTITION %s VALUES LESS THAN (UNIX_TIMESTAMP('%s')))",
		p.table,
		partitionName,
		partitionRange,
	)
	return p.db.Exec(sql).Error
}

// DropExpiredPartitions 删除过期分区
func (p *partition) DropExpiredPartitions(ctx context.Context, expiredMonths int) (err error) {
	if expiredMonths <= 0 {
		return nil
	}
	partitions, err := p.List(ctx)
	if err != nil {
		return err
	}
	// 删除过期的分区
	earliestPartition, _ := strconv.Atoi(strings.ReplaceAll(carbon.Now().SubMonths(expiredMonths-1).StartOfMonth().ToDateString(), "-", ""))
	for _, partition := range partitions {
		partitionNum, _ := strconv.Atoi(strings.TrimLeft(partition, "p"))
		if partitionNum < earliestPartition {
			sql := fmt.Sprintf(
				"ALTER TABLE %s DROP PARTITION p%d",
				p.table,
				partitionNum,
			)
			err = p.db.Exec(sql).Error
			if err != nil {
				logx.Error(context.Background(), fmt.Sprintf("drop  table %s partition %s failed", p.table, partition))
			}
		}
	}
	return err
}
