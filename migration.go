package gormx

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/itmisx/logx"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/plugin/dbresolver"
)

// 将Migration匿名嵌套到要迁移的结构体
// 通过AfterCreate,AfterUpdate,AfterDelete几个Hook进行双写
// 为避免匿名嵌套方法失效，原model定义避免使用afterCreate,afterUpdate,afterDelete几个方法
// 如果有可以命名为其他方法名称如 afterCreateOld afterUpdateOld afterDeleteOld，并在实例化migration时，通过参数传入，migration会自动执行他们
//
// 另加一个定时任务，进行历史数据的同步，为保证数据的一致性，同步时加锁
// 迁移表完成后，重命名原表为tableName.del 重命名临时表未原始表，采用原子操作

// type migrationTest struct {
// 	ID        int    `json:"id" gorm:"column:id;type:int(8);primaryKey;autoIncrement"`
// 	Name      string `json:"name" gorm:"column:name;type:varchar(20)"`
// 	Migration `gorm:"-"`
// }

// func (migrationTest) TableName() string {
// 	return "migration_test"
// }
// migration := NewMigration(
// 	db,
// 	"migration_test",
// 	nil, nil, nil,
// 	"alter table migration_test modify column c1 varchar(10)",
// )
// migration.Start()

type Migration struct {
	DB              *gorm.DB // gorm Engine
	Database        string   // 数据库名称
	TableName       string   // 原始表名称
	AfterCreateHook func(*gorm.DB) error
	AfterUpdateHook func(*gorm.DB) error
	AfterDeleteHook func(*gorm.DB) error
	AlterSQL        string // 执行变更的sql语句
}

type migrationLog struct {
	ID                 int64  `gorm:"column:id;type:bigint;primaryKey;autoIncrement;comment:主键"`
	OldTableName       string `gorm:"column:old_table_name;type:varchar(100);comment:原表名称"`
	NewTableName       string `gorm:"column:new_table_name;type:varchar(100);comment:新表名称"`
	OldTableBackupName string `gorm:"column:old_table_backup_name;type:varchar(100);comment:旧表归档备份名称"`
	StartID            int64  `gorm:"column:start_id;type:bigint;default:0;comment:开始同步的id"`
	EndID              int64  `gorm:"column:end_id;type:bigint;comment:结束同步的id"`
	TotalRecords       int64  `gorm:"column:total_records;type:bigint;comment:总的迁移条数"`
	CompletedRecords   int64  `gorm:"column:completed_records;type:bigint;comment:已完成的迁移条数"`
	Status             int    `gorm:"column:status;type:int;default:0;comment:迁移状态 0-未开始 1-进行中 2-已完成"`
	CreatedAt          int64  `gorm:"column:created_at;type:bigint;comment:创建时间"`
}

func (migrationLog) TableName() string {
	return "gorm_migration_log"
}

type migrationStatus int

const (
	migrationStatus_not_started = 0
	migrationStatus_in_progress = 1
	migrationStatus_completed   = 2
)

var migrationParams = make(map[string]*Migration)

func NewMigration(
	db *gorm.DB,
	tableName string,
	afterCreateHook func(*gorm.DB) error,
	afterUpdateHook func(*gorm.DB) error,
	afterDeleteHook func(*gorm.DB) error,
	alterSQL string,
) *Migration {
	m := &Migration{
		DB:              db,
		TableName:       tableName,
		AfterCreateHook: afterCreateHook,
		AfterUpdateHook: afterUpdateHook,
		AfterDeleteHook: afterDeleteHook,
		AlterSQL:        alterSQL,
	}
	migrationParams[tableName] = m
	return m
}

// 创建Hook
func (m *Migration) AfterCreate(tx *gorm.DB) error {
	stmt := tx.Statement
	_m := migrationParams[stmt.Table]
	newTable := _m.GetMigrateTempTable(migrationStatus_in_progress)
	if newTable != "" {
		if _, ok := tx.Statement.Settings.Load("after_create_done"); ok {
			return nil
		}
		dest := tx.Statement.Dest
		err := tx.Table(newTable).
			Session(&gorm.Session{SkipHooks: true}).
			Create(dest).Error
		tx.Statement.Settings.Store("after_create_done", 1)
		if err != nil {
			return err
		}
		if m.AfterCreateHook != nil {
			return m.AfterCreateHook(tx)
		}
		return nil
	}
	return nil
}

// 更新Hook
func (m *Migration) AfterUpdate(tx *gorm.DB) error {
	stmt := tx.Statement
	_m := migrationParams[stmt.Table]
	newTable := _m.GetMigrateTempTable(migrationStatus_in_progress)
	if newTable != "" {
		stmt := tx.Statement
		sql := tx.Dialector.Explain(stmt.SQL.String(), stmt.Vars...)
		err := tx.Session(&gorm.Session{SkipHooks: true}).Exec(sql).Error
		if err != nil {
			return err
		}
		if m.AfterUpdateHook != nil {
			return m.AfterUpdateHook(tx)
		}
		return nil
	}
	return nil
}

// 删除Hook
func (m *Migration) AfterDelete(tx *gorm.DB) error {
	stmt := tx.Statement
	_m := migrationParams[stmt.Table]
	newTable := _m.GetMigrateTempTable(migrationStatus_in_progress)
	if newTable != "" {
		stmt := tx.Statement
		sql := tx.Dialector.Explain(stmt.SQL.String(), stmt.Vars...)
		err := tx.Session(&gorm.Session{SkipHooks: true}).Exec(sql).Error
		if err != nil {
			return err
		}
		if m.AfterDeleteHook != nil {
			return m.AfterDeleteHook(tx)
		}
		return nil
	}
	return nil
}

// 获取迁移临时表
func (m *Migration) GetMigrateTempTable(status migrationStatus) string {
	if m == nil || m.DB == nil {
		return ""
	}
	var record migrationLog
	if !(m.DB.Clauses(dbresolver.Write).Migrator()).HasTable(&migrationLog{}) {
		m.DB.Clauses(dbresolver.Write).Migrator().AutoMigrate(&migrationLog{})
	}
	m.DB.Clauses(dbresolver.Write).Model(&migrationLog{}).
		Where("status = ?", int(status)).
		Order("id desc").
		Take(&record)
	return record.NewTableName
}

// Start 开始迁移
func (m *Migration) Start() error {
	// 迁移状态表，记录迁移中间表的名称，进行状态以及进度
	m.DB.Migrator().AutoMigrate(&migrationLog{})
	if m.AlterSQL == "" {
		return errors.New("no alter sql to exec")
	}
	// 先判断是否支持online ddl，即不阻塞DML的操作
	// 如
	// ALTER TABLE users
	// ADD COLUMN age INT,
	// DROP COLUMN old_address,
	// CHANGE COLUMN username user_name VARCHAR(100),
	// ADD INDEX idx_email (email),ALGORITHM=INPLACE, LOCK=NONE;
	// 如果不支持online ddl，mysql会报错
	alterSql := strings.TrimRight(m.AlterSQL, ";")
	alterSqlLower := strings.ToLower(alterSql)
	if strings.Contains(alterSqlLower, "partition by") {
		if strings.Contains(alterSqlLower, "add") ||
			strings.Contains(alterSqlLower, "drop") ||
			strings.Contains(alterSqlLower, "modify") ||
			strings.Contains(alterSqlLower, "change") {
			alterSql = strings.Replace(alterSql, "partition by", ",ALGORITHM=INPLACE, LOCK=NONE partition by", 1)
			alterSql = strings.Replace(alterSql, "PARTITION BY", ",ALGORITHM=INPLACE, LOCK=NONE PARTITION BY", 1)
		} else {
			alterSql = strings.Replace(alterSql, m.TableName, m.TableName+" ALGORITHM=INPLACE, LOCK=NONE", 1)
		}
	} else {
		alterSql = alterSql + ",ALGORITHM=INPLACE,LOCK=NONE;"
	}

	err := m.DB.Exec(alterSql).Error
	// 如果不支持才进行表复制迁移
	if err != nil {
		err := m.DB.Transaction(func(tx *gorm.DB) error {
			tx.Clauses(clause.Locking{Strength: "UPDATE"}).Find(&migrationLog{}) // for update保证顺序执行
			// 如果存在迁移中的任务，则停止，需等待先前的迁移任务完成才能继续
			var count int64
			tx.Model(&migrationLog{}).
				Where("old_table_name = ?", m.TableName).
				Where("status = ?", migrationStatus_in_progress).
				Count(&count)
			if count > 0 {
				logx.Warn(context.Background(), "wait old migration proccess completed")
				return nil
			}
			// 获取原表的创建语句
			var result map[string]interface{}
			err = tx.Raw("SHOW CREATE TABLE " + m.TableName).Scan(&result).Error
			if err != nil {
				return errors.New("show create table " + m.TableName + " failed")
			}
			oldTableDDL := result["Create Table"].(string)
			// 创建新表
			// 先复制原表结果
			newTableName := fmt.Sprintf(m.TableName+"_%d", time.Now().UnixMilli())
			copyTableDDL := strings.Replace(oldTableDDL, m.TableName, newTableName, 1)
			err = tx.Exec(copyTableDDL).Error
			if err != nil {
				return errors.New("create new table faile")
			}
			// 新表执行alter
			{
				alterSQL := strings.Replace(m.AlterSQL, m.TableName, newTableName, 1)
				err = tx.Exec(alterSQL).Error
				if err != nil {
					tx.Migrator().DropTable(newTableName)
					return errors.New("new table alter failed")
				}
			}
			// 确认新表是否真的发生结构变更
			// 如果没有变更，则删除新表
			{
				err = tx.Raw("SHOW CREATE TABLE " + newTableName).Scan(&result).Error
				if err != nil {
					return errors.New("show create new table " + newTableName + " failed")
				}
				newTableDDL := result["Create Table"].(string)
				if strings.Replace(newTableDDL, newTableName, m.TableName, 1) == oldTableDDL {
					tx.Migrator().DropTable(newTableName)
					return errors.New("there is no need to migrate")
				}
			}

			// 获取原表最大id
			var dstPrimary struct {
				ID int64 `gorm:"column:id"`
			}
			tx.Table(m.TableName).Order("id desc").Take(&dstPrimary)
			// 获取原表数据的总条数
			var total int64
			tx.Table(m.TableName).Count(&total)
			// 启动双写
			tx.Create(&migrationLog{
				OldTableName:     m.TableName,
				NewTableName:     newTableName,
				Status:           migrationStatus_in_progress,
				StartID:          0,
				EndID:            dstPrimary.ID,
				TotalRecords:     total,
				CompletedRecords: 0,
			})
			return nil
		})
		if err != nil {
			return err
		}
	}
	// 启动进程(迁移历史数据)
	go func() {
		for {
			// 判断表是否存在未完成的迁移任务
			if m.GetMigrateTempTable(migrationStatus_in_progress) == "" {
				break
			}
			// 启动迁移，事务保证数据完整性
			err := m.DB.Transaction(func(tx *gorm.DB) error {
				tx.Clauses(clause.Locking{Strength: "UPDATE"}).Find(&migrationLog{}) // for update保证顺序执行
				// 获取当前迁移进度
				var migratitonDetail migrationLog
				tx.Model(&migrationLog{}).
					Where("old_table_name = ?", m.TableName).
					Where("status = ?", migrationStatus_in_progress).
					Take(&migratitonDetail)
				// 获取新的200条
				var oldRecords []map[string]interface{}
				err := tx.Table(m.TableName).
					Where("id >= ? AND id <= ?", migratitonDetail.StartID, migratitonDetail.EndID).
					Order("id asc").
					Limit(200).Find(&oldRecords).Error
				if err != nil {
					logx.Error(context.Background(), "get old table records failed", logx.Err(err))
				} else {
					if len(oldRecords) > 0 && migratitonDetail.StartID != migratitonDetail.EndID { // 迁移中
						// 批量插入新的200条数据
						tx1 := tx.Table(migratitonDetail.NewTableName).
							Clauses(clause.OnConflict{
								Columns:   []clause.Column{{Name: "id"}},
								DoUpdates: clause.Assignments(map[string]interface{}{"id": gorm.Expr("id")}),
							}).
							Create(&oldRecords)
						if tx1.Error != nil {
							logx.Error(context.Background(), "migrate history records failed", logx.Err(err))
							return errors.New("migrate history records failed")
						}
						// 获取批次记录的最大id
						var maxID int64
						{
							val := reflect.ValueOf(oldRecords[len(oldRecords)-1]["id"])
							kind := val.Kind()
							if kind >= reflect.Int && kind <= reflect.Int64 {
								maxID = val.Int()
							} else if kind >= reflect.Uint && kind <= reflect.Uint64 {
								maxID = int64(val.Uint())
							}
						}
						// 更新已更新条数
						tx.Model(&migrationLog{}).
							Where("id = ?", migratitonDetail.ID).
							Updates(map[string]interface{}{
								"start_id":          maxID,
								"completed_records": gorm.Expr("completed_records + ?", tx1.RowsAffected),
							})
					} else { // 迁移完成
						// 更新迁移状态
						err := tx.Model(&migrationLog{}).
							Where("id = ?", migratitonDetail.ID).
							Update(
								"status",
								2,
							).Error
						if err != nil {
							logx.Error(context.Background(), "change migration status failed", logx.Err(err))
							return errors.New("change migration status failed")
						} else {
							// 备份表名称
							oldTableBackupName := fmt.Sprintf(m.TableName+"_old_%d", time.Now().UnixMilli())
							err := tx.Model(&migrationLog{}).
								Where("id = ?", migratitonDetail.ID).
								Update(
									"old_table_backup_name",
									oldTableBackupName,
								).Error
							if err != nil {
								return errors.New("save old table backup name failed")
							}
							// 切换工作表
							switchSQL := fmt.Sprintf("RENAME TABLE `%s` TO `%s`, `%s` TO `%s`",
								m.TableName, oldTableBackupName,
								migratitonDetail.NewTableName, m.TableName,
							)
							err = tx.Exec(switchSQL).Error
							if err != nil {
								return err
							}
						}
					}
				}
				return nil
			})
			if err != nil {
				break
			}
			// 休眠100ms
			time.Sleep(time.Millisecond * 100)
		}
	}()
	return nil
}
