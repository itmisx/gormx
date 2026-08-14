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
//
// &partition.Start() 会自动启动一个协程，定期自动创建分区和移除过期分区

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dromara/carbon/v2"
	"github.com/itmisx/logx"
	"github.com/samber/lo"
	"gorm.io/gorm"
)

type partition struct {
	db                *gorm.DB       // *gorm.DB
	database          string         // 数据库名
	table             string         // 表名
	partitionUnit     PartitionUnitT // 分区单位 1-按天 2-按月
	retentionDuration time.Duration  // 分区数据的保留时长
}

type PartitionUnitT int // 分区单元

const (
	PartitionUnitDay   PartitionUnitT = iota + 1 // 按天分区
	PartitionUnitMonth                           // 按月分区
	PartitionUnitYear                            // 按年分区
)

// 默认分区自动检查时间
var DefaultCronDuration = time.Hour

func NewPartition(
	db *gorm.DB,
	database string,
	table string,
	partitionUnit PartitionUnitT, // 分区单位
	retentionDuration time.Duration, // 数据保留时间
) *partition {
	return &partition{
		db:                db,
		database:          database,
		table:             table,
		partitionUnit:     partitionUnit,
		retentionDuration: retentionDuration,
	}
}

// List 获取所有分区
func (p *partition) list(ctx context.Context) (partitions []string, err error) {
	// 获取所有分区
	err = p.db.WithContext(ctx).Table("information_schema.PARTITIONS").
		Where("TABLE_SCHEMA = ?", p.database).
		Where("TABLE_NAME = ?", p.table).
		Where("PARTITION_NAME IS NOT NULL").
		Pluck("PARTITION_NAME", &partitions).
		Error
	return partitions, err
}

// ExistsDayPartition 是否存在日分区
func (p *partition) existsDayPartition(ctx context.Context, days int) (bool, error) {
	partitions, err := p.list(ctx)
	if err != nil {
		return false, err
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddDays(days).StartOfDay().ToDateString(), "-", "")
	return lo.Contains(partitions, partitionName), nil
}

// ExistsMonthPartition 是否存在月分区
func (p *partition) existsMonthPartition(ctx context.Context, months int) (bool, error) {
	partitions, err := p.list(ctx)
	if err != nil {
		return false, err
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddMonths(months).StartOfMonth().ToDateString(), "-", "")
	return lo.Contains(partitions, partitionName), nil
}

// ExistsYearPartition 是否存在年分区
func (p *partition) existsYearPartition(ctx context.Context, years int) (bool, error) {
	partitions, err := p.list(ctx)
	if err != nil {
		return false, err
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddYears(years).StartOfYear().ToDateString(), "-", "")
	return lo.Contains(partitions, partitionName), nil
}

// AddDayPartition 新增日分区
func (p *partition) addDayPartition(ctx context.Context, days int) error {
	exists, err := p.existsDayPartition(ctx, days)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddDays(days).StartOfDay().ToDateString(), "-", "")
	partitionRange := carbon.Now().AddDays(days).StartOfDay().ToDateString()
	createPartitionSQL := fmt.Sprintf(
		"ALTER TABLE %s ADD PARTITION(PARTITION %s VALUES LESS THAN (UNIX_TIMESTAMP('%s')))",
		p.table,
		partitionName,
		partitionRange,
	)
	return p.db.WithContext(ctx).Exec(createPartitionSQL).Error
}

// AddMonthPartition 新增月分区
func (p *partition) addMonthPartition(ctx context.Context, months int) error {
	exists, err := p.existsMonthPartition(ctx, months)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddMonths(months).StartOfMonth().ToDateString(), "-", "")
	partitionRange := carbon.Now().AddMonths(months).StartOfMonth().ToDateString()
	createPartitionSQL := fmt.Sprintf(
		"ALTER TABLE %s ADD PARTITION(PARTITION %s VALUES LESS THAN (UNIX_TIMESTAMP('%s')))",
		p.table,
		partitionName,
		partitionRange,
	)
	return p.db.WithContext(ctx).Exec(createPartitionSQL).Error
}

// AddYearPartition 新增年分区
func (p *partition) addYearPartition(ctx context.Context, years int) error {
	exists, err := p.existsYearPartition(ctx, years)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	partitionName := "p" + strings.ReplaceAll(carbon.Now().AddYears(years).StartOfYear().ToDateString(), "-", "")
	partitionRange := carbon.Now().AddYears(years).StartOfYear().ToDateString()
	sql := fmt.Sprintf(
		"ALTER TABLE %s ADD PARTITION(PARTITION %s VALUES LESS THAN (UNIX_TIMESTAMP('%s')))",
		p.table,
		partitionName,
		partitionRange,
	)
	return p.db.WithContext(ctx).Exec(sql).Error
}

// dropExpiredPartitions 删除过期分区
func (p *partition) dropExpiredPartitions(ctx context.Context) (err error) {
	if p.retentionDuration <= 0 {
		return nil
	}
	partitions, err := p.list(ctx)
	if err != nil {
		return err
	}
	// 删除过期的分区
	earliestPartition, _ := strconv.Atoi(strings.ReplaceAll(carbon.Now().SubNanoseconds(int(p.retentionDuration.Nanoseconds())).StartOfDay().ToDateString(), "-", ""))
	// 逐个删除，单个失败不影响其余分区，最后返回第一个错误
	var firstErr error
	for _, partition := range partitions {
		partitionNum, _ := strconv.Atoi(strings.TrimLeft(partition, "p"))
		if partitionNum >= earliestPartition {
			continue
		}
		sql := fmt.Sprintf(
			"ALTER TABLE %s DROP PARTITION p%d",
			p.table,
			partitionNum,
		)
		if err := p.db.WithContext(ctx).Exec(sql).Error; err != nil {
			logx.Error(ctx,
				fmt.Sprintf("drop table %s partition %s failed", p.table, partition),
				logx.Err(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// addNextPartitions 补齐未来两个周期的分区
func (p *partition) addNextPartitions(ctx context.Context) error {
	var add func(context.Context, int) error
	switch p.partitionUnit {
	case PartitionUnitDay:
		add = p.addDayPartition
	case PartitionUnitMonth:
		add = p.addMonthPartition
	case PartitionUnitYear:
		add = p.addYearPartition
	default:
		return fmt.Errorf("unsupported partition unit type: %d", p.partitionUnit)
	}
	for _, n := range []int{1, 2} {
		if err := add(ctx, n); err != nil {
			return fmt.Errorf("add partition of %s failed: %w", p.table, err)
		}
	}
	return nil
}

// Start 启动分区自动管理
// 以语句形式调用（p.Start()）时返回值可以忽略，不影响既有代码
func (p *partition) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	// 初始化
	// 分区单位不合法属于调用方的配置错误，返回错误而不是 panic 掉整个进程
	if err := p.addNextPartitions(ctx); err != nil {
		return err
	}
	// 定时检查，并自动创建分区，并删除过期的分区
	// 间隔取到局部变量里再钳制，避免多张表各自 Start 时并发读写包级变量
	interval := DefaultCronDuration
	if interval < time.Second*10 {
		interval = time.Second * 10
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			func() {
				ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second*30)
				defer cancel1()
				if err := p.addNextPartitions(ctx1); err != nil {
					logx.Error(ctx1, "add partition failed", logx.Err(err))
				}
				if err := p.dropExpiredPartitions(ctx1); err != nil {
					logx.Error(ctx1, "drop expired partition failed", logx.Err(err))
				}
			}()
		}
	}()
	return nil
}
