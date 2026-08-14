package gormx

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
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

// 迁移表参数
// Hook 会在每次 DML 时并发读取本 map，而 NewMigration 可能在任意时刻写入，
// 无保护的 map 并发读写会让 runtime 直接抛 "concurrent map read and map write" 并终止进程
var (
	migrationParamsMu sync.RWMutex
	migrationParams   = make(map[string]*Migration)
)

func getMigrationParam(table string) *Migration {
	migrationParamsMu.RLock()
	defer migrationParamsMu.RUnlock()
	return migrationParams[table]
}

func setMigrationParam(table string, m *Migration) {
	migrationParamsMu.Lock()
	defer migrationParamsMu.Unlock()
	migrationParams[table] = m
}

func listMigrationParams() []*Migration {
	migrationParamsMu.RLock()
	defer migrationParamsMu.RUnlock()
	ms := make([]*Migration, 0, len(migrationParams))
	for _, m := range migrationParams {
		ms = append(ms, m)
	}
	return ms
}

// 影子表名缓存，表名 -> 影子表名，空串表示该表当前没有迁移在进行
//
// Hook 每次 DML 都要判断所在表是否处于迁移中。若每次都查库，一次写入会多出 4 条查询
// （Migrator().HasTable 内部还要先 SELECT DATABASE() 并查 information_schema.SCHEMATA），
// 且这些查询都走主库，读写分离在这条路径上等于失效。
// 而绝大多数时间根本没有迁移在进行，这个代价是白付的。
// 因此把状态缓存在内存里，Hook 只读缓存：
// 本进程发起的迁移与切换会立即更新缓存，另有后台协程定期刷新，
// 以便感知其他实例发起或完成的迁移
var (
	shadowTableMu     sync.RWMutex
	shadowTables      = make(map[string]string)
	shadowRefreshOnce sync.Once
)

// ShadowTableRefreshInterval 影子表状态的刷新间隔
//
// 多实例部署时，其他实例发起迁移后，本实例最多延迟这么久才开始双写。
// 这段延迟是安全的：这期间的写入虽然没有双写，但都落在原表里，
// 随后会被历史数据回填带到影子表。真正不能有延迟的是切换工作表那一刻，
// 而切换由 RENAME 的排他元数据锁保证——它会等所有正在写这两张表的事务提交
var ShadowTableRefreshInterval = time.Second * 3

func loadShadowTable(table string) string {
	shadowTableMu.RLock()
	defer shadowTableMu.RUnlock()
	return shadowTables[table]
}

func storeShadowTable(table, shadow string) {
	shadowTableMu.Lock()
	defer shadowTableMu.Unlock()
	shadowTables[table] = shadow
}

// refreshShadowTables 刷新所有已注册表的迁移状态
func refreshShadowTables() {
	for _, m := range listMigrationParams() {
		storeShadowTable(m.TableName, m.GetMigrateTempTable(migrationStatus_in_progress))
	}
}

// startShadowTableRefresher 启动后台刷新协程，整个进程只启动一次
func startShadowTableRefresher() {
	shadowRefreshOnce.Do(func() {
		go func() {
			interval := ShadowTableRefreshInterval
			if interval < time.Second {
				interval = time.Second
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				refreshShadowTables()
			}
		}()
	})
}

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
	setMigrationParam(tableName, m)
	// 立即同步一次状态，这样进程重启后能接上此前未完成的迁移，
	// 不必等到第一次定时刷新
	storeShadowTable(tableName, m.GetMigrateTempTable(migrationStatus_in_progress))
	startShadowTableRefresher()
	return m
}

// 注意：Migration 是以匿名字段嵌入到业务 model 的（且标记了 gorm:"-"），
// gorm 通过 model 实例调用下面几个 Hook，所以方法内的 receiver m 永远是零值，
// m.DB / m.TableName / m.AfterXxxHook 全部不可用。
// Hook 内一律通过 migrationParams[stmt.Table] 取到 NewMigration 创建的真实实例。

// redirectTableSQL 把已构建好的语句重定向到影子表
// gorm 生成的 UPDATE/DELETE 会把目标表放在最前面且用反引号包裹，因此只替换第一次出现；
// 子查询中若引用了原表则保持不变（读取仍应落在原表）
func redirectTableSQL(rawSQL, oldTable, newTable string) (string, error) {
	if oldTable == "" || newTable == "" {
		return "", errors.New("empty table name, can not redirect sql")
	}
	quoted := "`" + oldTable + "`"
	if !strings.Contains(rawSQL, quoted) {
		// 定位不到目标表就必须放弃，否则语句会打回原表造成重复执行
		return "", fmt.Errorf("can not locate table %s in sql: %s", oldTable, rawSQL)
	}
	return strings.Replace(rawSQL, quoted, "`"+newTable+"`", 1), nil
}

// 双写会话
// NewDB 保证拿到一个干净的 Statement（不继承外层已构建的 SQL 与 Vars），
// 同时 ConnPool 仍是外层的连接/事务，双写与业务写入保持在同一个事务内
func doubleWriteSession(tx *gorm.DB) *gorm.DB {
	return tx.Session(&gorm.Session{NewDB: true, SkipHooks: true})
}

// 创建Hook
func (m *Migration) AfterCreate(tx *gorm.DB) error {
	stmt := tx.Statement
	_m := getMigrationParam(stmt.Table)
	// 读缓存，不查库：这是每次 DML 都会走到的热路径
	newTable := loadShadowTable(stmt.Table)
	if newTable != "" {
		if _, ok := tx.Statement.Settings.Load("after_create_done"); ok {
			return nil
		}
		dest := tx.Statement.Dest
		err := doubleWriteSession(tx).
			Table(newTable).
			Create(dest).Error
		tx.Statement.Settings.Store("after_create_done", 1)
		if err != nil && !isTableNotExist(err) {
			return err
		}
		if _m.AfterCreateHook != nil {
			return _m.AfterCreateHook(tx)
		}
		return nil
	}
	return nil
}

// 更新Hook
func (m *Migration) AfterUpdate(tx *gorm.DB) error {
	stmt := tx.Statement
	_m := getMigrationParam(stmt.Table)
	// 读缓存，不查库：这是每次 DML 都会走到的热路径
	newTable := loadShadowTable(stmt.Table)
	if newTable != "" {
		// 这里不能用 Table(newTable)，Exec 执行的是原始 SQL 文本，不会读取 Statement.Table
		sql, err := redirectTableSQL(stmt.SQL.String(), stmt.Table, newTable)
		if err != nil {
			return err
		}
		// 用 Vars 绑定参数，不要用 Dialector.Explain 插值（Explain 仅供日志使用，对二进制、时间精度有损）
		if err := doubleWriteSession(tx).Exec(sql, stmt.Vars...).Error; err != nil && !isTableNotExist(err) {
			return err
		}
		if _m.AfterUpdateHook != nil {
			return _m.AfterUpdateHook(tx)
		}
		return nil
	}
	return nil
}

// 删除Hook
func (m *Migration) AfterDelete(tx *gorm.DB) error {
	stmt := tx.Statement
	_m := getMigrationParam(stmt.Table)
	// 读缓存，不查库：这是每次 DML 都会走到的热路径
	newTable := loadShadowTable(stmt.Table)
	if newTable != "" {
		sql, err := redirectTableSQL(stmt.SQL.String(), stmt.Table, newTable)
		if err != nil {
			return err
		}
		if err := doubleWriteSession(tx).Exec(sql, stmt.Vars...).Error; err != nil && !isTableNotExist(err) {
			return err
		}
		if _m.AfterDeleteHook != nil {
			return _m.AfterDeleteHook(tx)
		}
		return nil
	}
	return nil
}

// 获取迁移临时表
// 必须按 old_table_name 过滤，否则任意一张表处于迁移中，
// 都会让其他表的 Hook 误判为自己在迁移，把数据双写到别人的影子表
func (m *Migration) GetMigrateTempTable(status migrationStatus) string {
	if m == nil || m.DB == nil || m.TableName == "" {
		return ""
	}
	// 迁移记录表由 NewMigration/Start 负责建立，这里不再做 HasTable + AutoMigrate：
	// 那两步会额外产生三条 information_schema 查询，而本方法会被定时刷新反复调用。
	// 表不存在时查询会报错并返回空串，语义上等价于“没有迁移在进行”
	var record migrationLog
	m.DB.Clauses(dbresolver.Write).Model(&migrationLog{}).
		Where("old_table_name = ?", m.TableName).
		Where("status = ?", int(status)).
		Order("id desc").
		Take(&record)
	return record.NewTableName
}

// MySQL 在不支持所请求的 online ddl 算法时返回的错误码
const (
	erAlterOperationNotSupported       = 1845 // ALGORITHM=INPLACE is not supported for this operation
	erAlterOperationNotSupportedReason = 1846 // ALGORITHM=INPLACE is not supported. Reason: ...
)

// erNoSuchTable 表不存在
const erNoSuchTable = 1146

// isTableNotExist 判断错误是否为“表不存在”
//
// 切换工作表时，RENAME 与随后的状态更新之间存在一个极短的窗口：
// 影子表已经改名成正式表，但迁移记录还写着进行中，
// 此时的业务写入会尝试往已经不存在的影子表名双写。
// 这个窗口内跳过双写是安全的——写入本来就已经落在切换后的正式表（即原影子表）上
func isTableNotExist(err error) bool {
	var myErr *mysqldriver.MySQLError
	return errors.As(err, &myErr) && myErr.Number == erNoSuchTable
}

// isOnlineDDLUnsupported 判断错误是否为 MySQL 明确报出的“不支持该 online ddl 算法”
//
// 只有这类错误才应该回退到复制表迁移。语法错误(1064)、列不存在(1054)、
// 权限不足、锁等待超时等都必须直接返回给调用方，
// 否则 AlterSQL 里一处笔误就会静默触发整张表的复制迁移
func isOnlineDDLUnsupported(err error) bool {
	var myErr *mysqldriver.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	return myErr.Number == erAlterOperationNotSupported ||
		myErr.Number == erAlterOperationNotSupportedReason
}

// 迁移准备阶段的互斥锁
const (
	migrateLockPrefix = "gormx:migrate:"
	// 拿不到锁说明别的实例正在准备迁移，直接跳过本轮，不等待
	migrateLockWaitSeconds = 0
	// MySQL 对用户锁名的长度限制是 64
	migrateLockNameMaxLen = 64
)

func (m *Migration) migrateLockName() string {
	name := migrateLockPrefix + m.TableName
	if len(name) > migrateLockNameMaxLen {
		sum := sha256.Sum256([]byte(m.TableName))
		name = migrateLockPrefix + hex.EncodeToString(sum[:])[:32]
	}
	return name
}

// withMigrateLock 在跨实例互斥的保护下执行 fn
//
// 这里不能用事务做互斥：MySQL 的 CREATE/ALTER/DROP/RENAME TABLE 都会隐式提交，
// 第一条 DDL 一执行，事务连同 SELECT ... FOR UPDATE 持有的锁就一起失效了，
// 之后的语句实际都运行在自动提交模式下。
// 改用连接级的用户锁 GET_LOCK：不受隐式提交影响，跨会话跨实例有效，
// 且进程崩溃导致连接断开时由 MySQL 自动释放。
//
// GET_LOCK / RELEASE_LOCK 必须在同一条连接上执行，所以这里从 database/sql
// 层独占一条连接专门用于加解锁——不能用 gorm 的 Connection()，
// dbresolver 只对事务(gorm.TxCommitter)放行，会把 *sql.Conn 换成连接池里的其他连接。
// 被保护的操作本身走哪条连接都可以，只要锁在此期间一直被持有。
func (m *Migration) withMigrateLock(fn func() error) error {
	sqlDB, err := m.DB.DB()
	if err != nil {
		return fmt.Errorf("get sql.DB failed: %w", err)
	}
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get dedicated connection failed: %w", err)
	}
	defer conn.Close()

	lockName := m.migrateLockName()
	var locked sql.NullInt64
	err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", lockName, migrateLockWaitSeconds).Scan(&locked)
	if err != nil {
		return fmt.Errorf("acquire migrate lock %s failed: %w", lockName, err)
	}
	if !locked.Valid || locked.Int64 != 1 {
		logx.Warn(ctx, "another migration on "+m.TableName+" is preparing, skip this round")
		return nil
	}
	defer func() {
		var released sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", lockName).Scan(&released); err != nil {
			logx.Error(ctx, "release migrate lock "+lockName+" failed", logx.Err(err))
		}
	}()
	return fn()
}

// 复制表迁移依赖整型 id 作为回填游标（回填按 id >= ? AND id <= ? 分批推进），
// 这里前置校验表结构，避免等到切换工作表的那一刻才发现不满足要求
func (m *Migration) checkIDColumn(tx *gorm.DB) error {
	var dataTypes []string
	err := tx.Table("information_schema.COLUMNS").
		Where("TABLE_SCHEMA = DATABASE()").
		Where("TABLE_NAME = ?", m.TableName).
		Where("COLUMN_NAME = ?", "id").
		Pluck("DATA_TYPE", &dataTypes).Error
	if err != nil {
		return fmt.Errorf("check id column of %s failed: %w", m.TableName, err)
	}
	if len(dataTypes) == 0 {
		return fmt.Errorf("table %s has no id column, copy migration requires an integer id", m.TableName)
	}
	switch strings.ToLower(dataTypes[0]) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		return nil
	default:
		return fmt.Errorf("table %s id column type is %s, copy migration requires an integer id", m.TableName, dataTypes[0])
	}
}

// prepareCopyMigration 准备复制表迁移：建影子表 -> 在影子表上执行变更 -> 登记迁移记录
// 登记完成后双写即刻生效，历史数据由 Start 启动的协程回填
//
// 调用方必须先持有迁移锁（见 withMigrateLock）。这里刻意不套事务：
// 中间的 CREATE/ALTER/DROP TABLE 都会隐式提交，事务不但没有保护作用，
// 还会让人误以为失败可以回滚。DDL 无法回滚，只能在失败时显式清理影子表。
func (m *Migration) prepareCopyMigration() error {
	// 如果存在迁移中的任务，则停止，需等待先前的迁移任务完成才能继续
	var count int64
	if err := m.DB.Model(&migrationLog{}).
		Where("old_table_name = ?", m.TableName).
		Where("status = ?", migrationStatus_in_progress).
		Count(&count).Error; err != nil {
		return fmt.Errorf("count in-progress migration of %s failed: %w", m.TableName, err)
	}
	if count > 0 {
		logx.Warn(context.Background(), "wait old migration proccess completed")
		return nil
	}
	// 建影子表之前先校验表结构，不满足直接失败，不留任何中间产物
	if err := m.checkIDColumn(m.DB); err != nil {
		return err
	}
	// 获取原表的创建语句
	var oldResult map[string]interface{}
	if err := m.DB.Raw("SHOW CREATE TABLE " + m.TableName).Scan(&oldResult).Error; err != nil {
		return errors.New("show create table " + m.TableName + " failed")
	}
	oldTableDDL, ok := oldResult["Create Table"].(string)
	if !ok {
		return errors.New("unexpected show create table result of " + m.TableName)
	}
	// 创建新表，先复制原表结构
	newTableName := fmt.Sprintf(m.TableName+"_%d", time.Now().UnixMilli())
	copyTableDDL := strings.Replace(oldTableDDL, m.TableName, newTableName, 1)
	if err := m.DB.Exec(copyTableDDL).Error; err != nil {
		return fmt.Errorf("create new table %s failed: %w", newTableName, err)
	}
	// 此后任何一步失败都要清掉已经建好的影子表
	dropShadow := func() {
		if err := m.DB.Migrator().DropTable(newTableName); err != nil {
			logx.Error(context.Background(), "drop shadow table "+newTableName+" failed", logx.Err(err))
		}
	}
	// 新表执行alter
	alterSQL := strings.Replace(m.AlterSQL, m.TableName, newTableName, 1)
	if err := m.DB.Exec(alterSQL).Error; err != nil {
		dropShadow()
		return fmt.Errorf("new table %s alter failed: %w", newTableName, err)
	}
	// 确认新表是否真的发生结构变更，没有变更则不需要迁移
	var newResult map[string]interface{}
	if err := m.DB.Raw("SHOW CREATE TABLE " + newTableName).Scan(&newResult).Error; err != nil {
		dropShadow()
		return errors.New("show create new table " + newTableName + " failed")
	}
	newTableDDL, ok := newResult["Create Table"].(string)
	if !ok {
		dropShadow()
		return errors.New("unexpected show create table result of " + newTableName)
	}
	if strings.Replace(newTableDDL, newTableName, m.TableName, 1) == oldTableDDL {
		dropShadow()
		return errors.New("there is no need to migrate")
	}
	// 获取原表最大id，作为历史数据回填的终点
	// 这里的错误必须显式处理：一旦失败而 EndID 静默留在 0，
	// 回填首轮就会满足 StartID == EndID 而判定“迁移完成”，
	// 直接把空的影子表切换成正式表
	// 原表为空时 ErrRecordNotFound 属于正常情况，EndID 为 0 即是正确结果
	var dstPrimary struct {
		ID int64 `gorm:"column:id"`
	}
	if err := m.DB.Table(m.TableName).Order("id desc").Take(&dstPrimary).Error; err != nil &&
		!errors.Is(err, gorm.ErrRecordNotFound) {
		dropShadow()
		return fmt.Errorf("get max id of %s failed: %w", m.TableName, err)
	}
	// 获取原表数据的总条数
	var total int64
	if err := m.DB.Table(m.TableName).Count(&total).Error; err != nil {
		dropShadow()
		return fmt.Errorf("count %s failed: %w", m.TableName, err)
	}
	// 登记迁移记录，双写从这一刻开始生效
	if err := m.DB.Create(&migrationLog{
		OldTableName:     m.TableName,
		NewTableName:     newTableName,
		Status:           migrationStatus_in_progress,
		StartID:          0,
		EndID:            dstPrimary.ID,
		TotalRecords:     total,
		CompletedRecords: 0,
	}).Error; err != nil {
		dropShadow()
		return fmt.Errorf("create migration log failed: %w", err)
	}
	// 本实例立即开始双写，不必等定时刷新
	storeShadowTable(m.TableName, newTableName)
	return nil
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
		if strings.Contains(alterSqlLower, "add ") ||
			strings.Contains(alterSqlLower, "drop ") ||
			strings.Contains(alterSqlLower, "modify ") ||
			strings.Contains(alterSqlLower, "change ") {
			alterSql = strings.Replace(alterSql, "partition by", ",ALGORITHM=INPLACE, LOCK=NONE partition by", 1)
			alterSql = strings.Replace(alterSql, "PARTITION BY", ",ALGORITHM=INPLACE, LOCK=NONE PARTITION BY", 1)
		} else {
			alterSql = strings.Replace(alterSql, m.TableName, m.TableName+" ALGORITHM=INPLACE, LOCK=NONE", 1)
		}
	} else {
		alterSql = alterSql + ",ALGORITHM=INPLACE,LOCK=NONE;"
	}

	err := m.DB.Exec(alterSql).Error
	// 只有 MySQL 明确报出“不支持该 online ddl 算法”时才回退到复制表迁移
	if err != nil {
		if !isOnlineDDLUnsupported(err) {
			return fmt.Errorf("online ddl of %s failed: %w", m.TableName, err)
		}
		if err := m.withMigrateLock(m.prepareCopyMigration); err != nil {
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
				// 加共享锁，避免以下时序导致影子表永久停留在旧值：
				// 回填读到旧值 -> 业务更新原表并双写影子表(此时行还没插入，影响 0 行)
				// -> 回填把刚读到的旧值插入影子表。
				// 业务事务会先持有源行的排他锁，加共享锁后这里会等它提交，读到的就是新值
				var oldRecords []map[string]interface{}
				err := tx.Table(m.TableName).
					Clauses(clause.Locking{Strength: "SHARE"}).
					Where("id >= ? AND id <= ?", migratitonDetail.StartID, migratitonDetail.EndID).
					Order("id asc").
					Limit(200).Find(&oldRecords).Error
				if err != nil {
					logx.Error(context.Background(), "get old table records failed", logx.Err(err))
				} else {
					// 本轮没有数据可回填即视为完成
					finished := len(oldRecords) == 0
					if !finished { // 迁移中
						// 批量插入新的200条数据
						tx1 := tx.Table(migratitonDetail.NewTableName).
							Clauses(clause.OnConflict{
								Columns:   []clause.Column{{Name: "id"}},
								DoUpdates: clause.Assignments(map[string]interface{}{"id": gorm.Expr("id")}),
							}).
							Create(&oldRecords)
						if tx1.Error != nil {
							logx.Error(context.Background(), "migrate history records failed", logx.Err(tx1.Error))
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
						if maxID <= migratitonDetail.StartID {
							// 游标无法继续推进，说明 [StartID, EndID] 区间内只剩已回填过的那一行
							// （典型场景是 EndID 对应的行在快照之后被删除）。
							// 此时若继续循环，每轮都会读到同一行、游标原地不动，
							// 迁移永远不会结束，只是以 100ms 为周期空转
							finished = true
						} else {
							// 更新已更新条数
							tx.Model(&migrationLog{}).
								Where("id = ?", migratitonDetail.ID).
								Updates(map[string]interface{}{
									"start_id":          maxID,
									"completed_records": gorm.Expr("completed_records + ?", tx1.RowsAffected),
								})
						}
					}
					if finished { // 迁移完成
						// 切换前的兜底校验：原表有数据而影子表为空，
						// 说明回填从未真正执行，此时切换会让线上表瞬间变空。
						// 必须放在更新状态之前——RENAME 是 DDL，会隐式提交前面的更新，
						// 一旦走到那一步就回滚不掉了
						var srcCount, dstCount int64
						if err := tx.Table(m.TableName).Count(&srcCount).Error; err != nil {
							return fmt.Errorf("count %s failed: %w", m.TableName, err)
						}
						if err := tx.Table(migratitonDetail.NewTableName).Count(&dstCount).Error; err != nil {
							return fmt.Errorf("count %s failed: %w", migratitonDetail.NewTableName, err)
						}
						if srcCount > 0 && dstCount == 0 {
							logx.Error(context.Background(), fmt.Sprintf(
								"refuse to switch table %s: source has %d rows but %s is empty",
								m.TableName, srcCount, migratitonDetail.NewTableName,
							))
							return fmt.Errorf("refuse to switch table %s: shadow table %s is empty",
								m.TableName, migratitonDetail.NewTableName)
						}
						if srcCount != dstCount {
							// 不阻断切换：新增唯一索引等变更会合法地减少行数，
							// 但两边不一致值得记录下来供事后核对
							logx.Warn(context.Background(), fmt.Sprintf(
								"table %s row count mismatch before switch: source=%d shadow=%d",
								m.TableName, srcCount, dstCount,
							))
						}
						// 必须先切换工作表、再标记完成，顺序不能颠倒。
						//
						// RENAME 是 DDL，会先隐式提交前面的 DML，再去申请两张表的排他元数据锁，
						// 而这把锁要等所有正在写这两张表的事务提交后才能拿到。
						// 若先把状态改成已完成，那条 UPDATE 会随隐式提交立刻对外可见，
						// 于是这段等待期内的业务写入：语句仍然落在旧表（尚未改名），
						// Hook 却查到“已完成”而跳过双写 —— 这些更新只留在旧表里，
						// 旧表随即变成备份表，线上表就永久停留在旧值
						oldTableBackupName := fmt.Sprintf(m.TableName+"_old_%d", time.Now().UnixMilli())
						switchSQL := fmt.Sprintf("RENAME TABLE `%s` TO `%s`, `%s` TO `%s`",
							m.TableName, oldTableBackupName,
							migratitonDetail.NewTableName, m.TableName,
						)
						if err := tx.Exec(switchSQL).Error; err != nil {
							return err
						}
						if err := tx.Model(&migrationLog{}).
							Where("id = ?", migratitonDetail.ID).
							Updates(map[string]interface{}{
								"status":                migrationStatus_completed,
								"old_table_backup_name": oldTableBackupName,
							}).Error; err != nil {
							logx.Error(context.Background(), "change migration status failed", logx.Err(err))
							return errors.New("change migration status failed")
						}
						// 切换已完成，立即停止双写
						storeShadowTable(m.TableName, "")
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
