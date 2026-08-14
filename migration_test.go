package gormx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// migration 相关测试
//
// 需要一个本地 MySQL（与 gormx_test.go 使用同一个实例），连不上时自动 Skip。
// 每个用例使用独立的数据库 gormx_migration_test，跑完自动删除，不影响其他库。
//
//	go test -run TestMigration -v

const (
	testMySQLDSN = "root:123456@tcp(127.0.0.1:13306)/"
	testDatabase = "gormx_migration_test"
)

// ---------------------------------------------------------------- 测试用 model

// 双写用例
type dwUser struct {
	ID        int    `gorm:"column:id;primaryKey;autoIncrement"`
	Name      string `gorm:"column:name;type:varchar(50)"`
	N         int    `gorm:"column:n;type:int;default:0"`
	Migration `gorm:"-"`
}

func (dwUser) TableName() string { return "dw_user" }

// 跨表隔离用例：另一张同样嵌入了 Migration、但并未处于迁移中的表
type dwOther struct {
	ID        int    `gorm:"column:id;primaryKey;autoIncrement"`
	Name      string `gorm:"column:name;type:varchar(50)"`
	N         int    `gorm:"column:n;type:int;default:0"`
	Migration `gorm:"-"`
}

func (dwOther) TableName() string { return "dw_other" }

// Start() 用例
type stUser struct {
	ID        int    `gorm:"column:id;primaryKey;autoIncrement"`
	Name      string `gorm:"column:name;type:varchar(50)"`
	Migration `gorm:"-"`
}

func (stUser) TableName() string { return "st_user" }

// ---------------------------------------------------------------- 测试辅助

// limitPool 限制单个连接池规模
// 每个用例都会新建连接池，若不加限制也不回收，跑 -count=2 时会撞上 max_connections
func limitPool(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("获取 sql.DB 失败：%v", err)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(2)
}

// newTestDB 建一个干净的独立库，测试结束后删除并归还连接
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	root, err := gorm.Open(mysql.Open(testMySQLDSN+"?charset=utf8mb4"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("跳过：连接不上本地 MySQL(%s)：%v", testMySQLDSN, err)
	}
	limitPool(t, root)
	if err := root.Exec("DROP DATABASE IF EXISTS " + testDatabase).Error; err != nil {
		t.Skipf("跳过：无法准备测试库：%v", err)
	}
	if err := root.Exec("CREATE DATABASE " + testDatabase).Error; err != nil {
		t.Fatalf("创建测试库失败：%v", err)
	}

	db, err := gorm.Open(mysql.Open(testMySQLDSN+testDatabase+"?charset=utf8mb4&parseTime=true"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接测试库失败：%v", err)
	}
	limitPool(t, db)

	t.Cleanup(func() {
		// 先关业务连接池，避免残留的回填协程继续持有连接妨碍 DROP DATABASE
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
		root.Exec("DROP DATABASE IF EXISTS " + testDatabase)
		if sqlDB, err := root.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return db
}

// createTable 建表，columns 形如 "id INT ..., name VARCHAR(50)"
func createTable(t *testing.T, db *gorm.DB, name, columns string) {
	t.Helper()
	if err := db.Exec(fmt.Sprintf("CREATE TABLE `%s` (%s)", name, columns)).Error; err != nil {
		t.Fatalf("建表 %s 失败：%v", name, err)
	}
}

const dwColumns = "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50), n INT DEFAULT 0"

// markMigrating 写入一条“迁移进行中”的记录，等价于 Start() 回退分支所做的登记
func markMigrating(t *testing.T, db *gorm.DB, oldTable, newTable string) {
	t.Helper()
	if err := db.Migrator().AutoMigrate(&migrationLog{}); err != nil {
		t.Fatalf("创建 %s 失败：%v", migrationLog{}.TableName(), err)
	}
	err := db.Create(&migrationLog{
		OldTableName: oldTable,
		NewTableName: newTable,
		Status:       migrationStatus_in_progress,
	}).Error
	if err != nil {
		t.Fatalf("写入迁移记录失败：%v", err)
	}
}

func rowCount(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var c int64
	if err := db.Table(table).Count(&c).Error; err != nil {
		t.Fatalf("统计 %s 行数失败：%v", table, err)
	}
	return c
}

func dumpRows(t *testing.T, db *gorm.DB, table string) []map[string]interface{} {
	t.Helper()
	var rows []map[string]interface{}
	db.Table(table).Order("id asc").Find(&rows)
	return rows
}

// assertSameRows 断言两张表内容完全一致
func assertSameRows(t *testing.T, db *gorm.DB, orig, ghost string) {
	t.Helper()
	o, g := dumpRows(t, db, orig), dumpRows(t, db, ghost)
	if fmt.Sprint(o) != fmt.Sprint(g) {
		t.Errorf("两表内容不一致\n  %s = %v\n  %s = %v", orig, o, ghost, g)
	}
}

// waitFor 轮询等待条件成立
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时(%s)：%s", timeout, desc)
}

// ---------------------------------------------------------------- 纯单元测试

func TestRedirectTableSQL(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		oldTable string
		newTable string
		want     string
		wantErr  bool
	}{
		{
			name:     "UPDATE 重定向",
			sql:      "UPDATE `dw_user` SET `n`=? WHERE id = ?",
			oldTable: "dw_user", newTable: "dw_user_ghost",
			want: "UPDATE `dw_user_ghost` SET `n`=? WHERE id = ?",
		},
		{
			name:     "DELETE 重定向",
			sql:      "DELETE FROM `dw_user` WHERE id = ?",
			oldTable: "dw_user", newTable: "dw_user_ghost",
			want: "DELETE FROM `dw_user_ghost` WHERE id = ?",
		},
		{
			name:     "只替换第一次出现，子查询仍读原表",
			sql:      "DELETE FROM `dw_user` WHERE id IN (SELECT id FROM `dw_user` WHERE n > ?)",
			oldTable: "dw_user", newTable: "dw_user_ghost",
			want: "DELETE FROM `dw_user_ghost` WHERE id IN (SELECT id FROM `dw_user` WHERE n > ?)",
		},
		{
			name:     "表名是其他标识符的前缀时不会误替换",
			sql:      "UPDATE `dw_user` SET `dw_user_name`=? WHERE id = ?",
			oldTable: "dw_user", newTable: "ghost",
			want: "UPDATE `ghost` SET `dw_user_name`=? WHERE id = ?",
		},
		{
			name:     "定位不到表名必须报错，不能放行",
			sql:      "UPDATE `other_table` SET `n`=? WHERE id = ?",
			oldTable: "dw_user", newTable: "dw_user_ghost",
			wantErr: true,
		},
		{
			name:     "未加反引号的表名不认，避免误伤字符串字面量",
			sql:      "UPDATE dw_user SET n=? WHERE id = ?",
			oldTable: "dw_user", newTable: "dw_user_ghost",
			wantErr: true,
		},
		{
			name: "空的原表名",
			sql:  "UPDATE `dw_user` SET `n`=?",
			// oldTable 为空
			newTable: "dw_user_ghost",
			wantErr:  true,
		},
		{
			name:     "空的影子表名",
			sql:      "UPDATE `dw_user` SET `n`=?",
			oldTable: "dw_user",
			wantErr:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := redirectTableSQL(c.sql, c.oldTable, c.newTable)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际返回 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期错误：%v", err)
			}
			if got != c.want {
				t.Errorf("重定向结果不符\n  期望 %q\n  实际 %q", c.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------- 双写

func TestMigrationDualWriteCreate(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost")
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")

	t.Run("单条插入", func(t *testing.T) {
		if err := db.Create(&dwUser{Name: "a", N: 1}).Error; err != nil {
			t.Fatalf("插入失败：%v", err)
		}
		if o, g := rowCount(t, db, "dw_user"), rowCount(t, db, "dw_user_ghost"); o != 1 || g != 1 {
			t.Fatalf("期望两表各 1 行，实际 原表=%d 影子=%d", o, g)
		}
		assertSameRows(t, db, "dw_user", "dw_user_ghost")
	})

	// 批量插入时 AfterCreate 会被逐行回调，但 Statement.Dest 是整个切片，
	// 靠 after_create_done 标记避免把整批数据重复插入 N 次
	t.Run("批量插入不重复", func(t *testing.T) {
		rows := []dwUser{{Name: "b"}, {Name: "c"}, {Name: "d"}}
		if err := db.Create(&rows).Error; err != nil {
			t.Fatalf("批量插入失败：%v", err)
		}
		if o, g := rowCount(t, db, "dw_user"), rowCount(t, db, "dw_user_ghost"); o != 4 || g != 4 {
			t.Fatalf("期望两表各 4 行，实际 原表=%d 影子=%d", o, g)
		}
		assertSameRows(t, db, "dw_user", "dw_user_ghost")
	})
}

func TestMigrationDualWriteUpdate(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost")
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")

	rows := []dwUser{{Name: "a", N: 1}, {Name: "b", N: 1}, {Name: "c", N: 1}}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}

	t.Run("单行更新", func(t *testing.T) {
		err := db.Model(&dwUser{}).Where("id = ?", rows[0].ID).Update("name", "a2").Error
		if err != nil {
			t.Fatalf("更新失败：%v", err)
		}
		assertSameRows(t, db, "dw_user", "dw_user_ghost")
	})

	// n = n + 1 这类表达式不是幂等的，如果双写落回原表会被执行两次
	t.Run("非幂等表达式只执行一次", func(t *testing.T) {
		err := db.Model(&dwUser{}).Where("id = ?", rows[0].ID).
			Update("n", gorm.Expr("n + 1")).Error
		if err != nil {
			t.Fatalf("更新失败：%v", err)
		}
		var n int
		db.Table("dw_user").Where("id = ?", rows[0].ID).Select("n").Scan(&n)
		if n != 2 {
			t.Errorf("原表 n 期望 2（1+1），实际 %d —— 说明该语句在原表上被执行了多次", n)
		}
		assertSameRows(t, db, "dw_user", "dw_user_ghost")
	})

	t.Run("多行更新", func(t *testing.T) {
		err := db.Model(&dwUser{}).Where("n > ?", 0).
			Update("n", gorm.Expr("n + 10")).Error
		if err != nil {
			t.Fatalf("更新失败：%v", err)
		}
		var sum int
		db.Table("dw_user").Select("COALESCE(SUM(n),0)").Scan(&sum)
		if sum != 34 { // (2+10) + (1+10) + (1+10)
			t.Errorf("原表 sum(n) 期望 34，实际 %d", sum)
		}
		assertSameRows(t, db, "dw_user", "dw_user_ghost")
	})
}

func TestMigrationDualWriteDelete(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost")
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")

	rows := []dwUser{{Name: "a", N: 1}, {Name: "b", N: 2}, {Name: "c", N: 3}}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}

	t.Run("单行删除", func(t *testing.T) {
		if err := db.Where("id = ?", rows[0].ID).Delete(&dwUser{}).Error; err != nil {
			t.Fatalf("删除失败：%v", err)
		}
		if o, g := rowCount(t, db, "dw_user"), rowCount(t, db, "dw_user_ghost"); o != 2 || g != 2 {
			t.Fatalf("期望两表各 2 行，实际 原表=%d 影子=%d", o, g)
		}
		assertSameRows(t, db, "dw_user", "dw_user_ghost")
	})

	t.Run("多行删除", func(t *testing.T) {
		if err := db.Where("n > ?", 0).Delete(&dwUser{}).Error; err != nil {
			t.Fatalf("删除失败：%v", err)
		}
		if o, g := rowCount(t, db, "dw_user"), rowCount(t, db, "dw_user_ghost"); o != 0 || g != 0 {
			t.Fatalf("期望两表清空，实际 原表=%d 影子=%d", o, g)
		}
	})
}

// Migration 是以匿名字段嵌入 model 的，Hook 内的 receiver 恒为零值，
// 用户回调必须从 migrationParams 取真实实例才能被执行
func TestMigrationUserHooks(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost")

	fired := map[string]int{}
	NewMigration(db, "dw_user",
		func(*gorm.DB) error { fired["create"]++; return nil },
		func(*gorm.DB) error { fired["update"]++; return nil },
		func(*gorm.DB) error { fired["delete"]++; return nil },
		"alter table dw_user add column c1 int",
	)

	row := &dwUser{Name: "a"}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if err := db.Model(&dwUser{}).Where("id = ?", row.ID).Update("name", "a2").Error; err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if err := db.Where("id = ?", row.ID).Delete(&dwUser{}).Error; err != nil {
		t.Fatalf("删除失败：%v", err)
	}

	for _, k := range []string{"create", "update", "delete"} {
		if fired[k] != 1 {
			t.Errorf("%s 回调期望执行 1 次，实际 %d 次", k, fired[k])
		}
	}
}

// 用户回调返回错误时，应当中断本次写入
func TestMigrationUserHookError(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost")
	NewMigration(db, "dw_user",
		func(*gorm.DB) error { return fmt.Errorf("业务回调失败") },
		nil, nil,
		"alter table dw_user add column c1 int",
	)

	err := db.Create(&dwUser{Name: "a"}).Error
	if err == nil || !strings.Contains(err.Error(), "业务回调失败") {
		t.Fatalf("期望回调错误向上传播，实际 err=%v", err)
	}
	if o := rowCount(t, db, "dw_user"); o != 0 {
		t.Errorf("回调失败后应回滚，原表期望 0 行，实际 %d 行", o)
	}
}

// 没有迁移进行中时，业务读写完全不受影响，也不应产生任何影子表写入
func TestMigrationNoMigrationInProgress(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	// 只建表，不写 migrationLog 记录
	if err := db.Migrator().AutoMigrate(&migrationLog{}); err != nil {
		t.Fatal(err)
	}
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")

	row := &dwUser{Name: "a", N: 1}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if err := db.Model(&dwUser{}).Where("id = ?", row.ID).Update("n", gorm.Expr("n + 1")).Error; err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if err := db.Where("id = ?", row.ID).Delete(&dwUser{}).Error; err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if g := rowCount(t, db, "dw_user_ghost"); g != 0 {
		t.Errorf("无迁移时影子表不应有写入，实际 %d 行", g)
	}
}

// 只有一张表在迁移时，其他同样嵌入了 Migration 的表不能被误判
func TestMigrationCrossTableIsolation(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	createTable(t, db, "dw_other", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost") // 只有 dw_user 在迁移
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")
	NewMigration(db, "dw_other", nil, nil, nil, "alter table dw_other add column c1 int")

	// 每一步都要断言：若只在最后检查，dw_other 串进影子表的行会被随后的
	// delete 一并删掉，计数回到 0，反而掩盖了污染
	assertGhostClean := func(step string) {
		t.Helper()
		if g := rowCount(t, db, "dw_user_ghost"); g != 0 {
			t.Errorf("dw_other 的 %s 串到了 dw_user 的影子表：%v",
				step, dumpRows(t, db, "dw_user_ghost"))
		}
	}
	other := &dwOther{Name: "x", N: 1}
	if err := db.Create(other).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	assertGhostClean("create")
	if err := db.Model(&dwOther{}).Where("id = ?", other.ID).Update("name", "x2").Error; err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	assertGhostClean("update")
	if err := db.Where("id = ?", other.ID).Delete(&dwOther{}).Error; err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	assertGhostClean("delete")

	// 真正在迁移的表，双写依然正常
	if err := db.Create(&dwUser{Name: "y"}).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if o, g := rowCount(t, db, "dw_user"), rowCount(t, db, "dw_user_ghost"); o != 1 || g != 1 {
		t.Errorf("dw_user 双写异常：原表=%d 影子=%d", o, g)
	}
}

// ---------------------------------------------------------------- 影子表状态缓存

// countingLogger 统计执行过的 SQL 条数
type countingLogger struct {
	logger.Interface
	mu  sync.Mutex
	sql []string
}

func (l *countingLogger) Trace(ctx context.Context, begin time.Time,
	fc func() (string, int64), err error) {
	sql, _ := fc()
	l.mu.Lock()
	l.sql = append(l.sql, sql)
	l.mu.Unlock()
}

func (l *countingLogger) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sql = nil
}

func (l *countingLogger) statements() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.sql...)
}

// Hook 是每次 DML 都会走到的热路径，没有迁移进行时不允许产生任何额外查询
func TestMigrationHookNoExtraQuery(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	if err := db.Migrator().AutoMigrate(&migrationLog{}); err != nil {
		t.Fatal(err)
	}
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")

	counter := &countingLogger{Interface: logger.Default.LogMode(logger.Silent)}
	db.Logger = counter

	row := &dwUser{Name: "a"}
	for _, c := range []struct {
		name string
		do   func() error
	}{
		{"INSERT", func() error { return db.Create(row).Error }},
		{"UPDATE", func() error {
			return db.Model(&dwUser{}).Where("id = ?", row.ID).Update("name", "b").Error
		}},
		{"DELETE", func() error { return db.Where("id = ?", row.ID).Delete(&dwUser{}).Error }},
	} {
		counter.reset()
		if err := c.do(); err != nil {
			t.Fatalf("%s 失败：%v", c.name, err)
		}
		if got := counter.statements(); len(got) != 1 {
			t.Errorf("%s 期望只执行 1 条 SQL，实际 %d 条：%v", c.name, len(got), got)
		}
	}
}

// 其他实例发起的迁移，本实例要能通过定期刷新感知到并开始双写
func TestMigrationCachePicksUpOtherInstance(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	if err := db.Migrator().AutoMigrate(&migrationLog{}); err != nil {
		t.Fatal(err)
	}
	// 注册时还没有任何迁移
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")
	if err := db.Create(&dwUser{Name: "before"}).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if g := rowCount(t, db, "dw_user_ghost"); g != 0 {
		t.Fatalf("此时不应双写，影子表实际 %d 行", g)
	}

	// 另一个实例发起了迁移（直接写入迁移记录）
	if err := db.Create(&migrationLog{
		OldTableName: "dw_user", NewTableName: "dw_user_ghost",
		Status: migrationStatus_in_progress,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 刷新之前本实例还感知不到
	if err := db.Create(&dwUser{Name: "during"}).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if g := rowCount(t, db, "dw_user_ghost"); g != 0 {
		t.Errorf("刷新前不应双写，影子表实际 %d 行", g)
	}
	// 刷新后开始双写（后台协程按 ShadowTableRefreshInterval 做同样的事）
	refreshShadowTables()
	if err := db.Create(&dwUser{Name: "after"}).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if g := rowCount(t, db, "dw_user_ghost"); g != 1 {
		t.Errorf("刷新后应开始双写，影子表期望 1 行，实际 %d 行", g)
	}

	// 迁移完成后同样要能感知到并停止双写
	if err := db.Model(&migrationLog{}).Where("old_table_name = ?", "dw_user").
		Update("status", migrationStatus_completed).Error; err != nil {
		t.Fatal(err)
	}
	refreshShadowTables()
	if err := db.Create(&dwUser{Name: "done"}).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if g := rowCount(t, db, "dw_user_ghost"); g != 1 {
		t.Errorf("迁移完成后应停止双写，影子表期望仍是 1 行，实际 %d 行", g)
	}
}

// 进程重启后要能接上此前未完成的迁移，不必等定时刷新
func TestMigrationCacheInitOnRegister(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost")
	// 注册即刻生效，中间没有任何刷新
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")
	if err := db.Create(&dwUser{Name: "a"}).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if g := rowCount(t, db, "dw_user_ghost"); g != 1 {
		t.Errorf("注册后应立即开始双写，影子表期望 1 行，实际 %d 行", g)
	}
}

// ---------------------------------------------------------------- 迁移状态查询

func TestGetMigrateTempTable(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrator().AutoMigrate(&migrationLog{}); err != nil {
		t.Fatal(err)
	}
	db.Create(&migrationLog{OldTableName: "t_a", NewTableName: "t_a_ghost", Status: migrationStatus_in_progress})
	db.Create(&migrationLog{OldTableName: "t_b", NewTableName: "t_b_ghost", Status: migrationStatus_completed})

	ma := &Migration{DB: db, TableName: "t_a"}
	mb := &Migration{DB: db, TableName: "t_b"}
	mc := &Migration{DB: db, TableName: "t_c"}

	if got := ma.GetMigrateTempTable(migrationStatus_in_progress); got != "t_a_ghost" {
		t.Errorf("t_a 进行中的影子表期望 t_a_ghost，实际 %q", got)
	}
	// 状态不匹配
	if got := ma.GetMigrateTempTable(migrationStatus_completed); got != "" {
		t.Errorf("t_a 没有已完成记录，期望空，实际 %q", got)
	}
	// 已完成的表不应被当作迁移中
	if got := mb.GetMigrateTempTable(migrationStatus_in_progress); got != "" {
		t.Errorf("t_b 已完成，不应返回影子表，实际 %q", got)
	}
	if got := mb.GetMigrateTempTable(migrationStatus_completed); got != "t_b_ghost" {
		t.Errorf("t_b 已完成的影子表期望 t_b_ghost，实际 %q", got)
	}
	// 不相干的表不能读到别人的记录
	if got := mc.GetMigrateTempTable(migrationStatus_in_progress); got != "" {
		t.Errorf("t_c 未登记任何迁移，期望空，实际 %q", got)
	}
	// 防御性分支
	if got := (*Migration)(nil).GetMigrateTempTable(migrationStatus_in_progress); got != "" {
		t.Errorf("nil receiver 期望空，实际 %q", got)
	}
	if got := (&Migration{DB: db}).GetMigrateTempTable(migrationStatus_in_progress); got != "" {
		t.Errorf("表名为空时期望空，实际 %q", got)
	}
}

// ---------------------------------------------------------------- Start

func TestMigrationStartWithoutAlterSQL(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	m := NewMigration(db, "st_user", nil, nil, nil, "")
	if err := m.Start(); err == nil {
		t.Fatal("AlterSQL 为空时期望返回错误")
	}
}

// 支持 online ddl 时直接执行，不应留下任何迁移记录（不走复制表流程）
func TestMigrationStartOnlineDDL(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	if err := db.Create(&stUser{Name: "a"}).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}

	m := NewMigration(db, "st_user", nil, nil, nil, "alter table st_user add column age int")
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	if !db.Migrator().HasColumn(&stUser{}, "age") {
		t.Error("age 字段未添加，online ddl 未生效")
	}
	var logs int64
	db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Count(&logs)
	if logs != 0 {
		t.Errorf("online ddl 成功时不应产生迁移记录，实际 %d 条", logs)
	}
	if n := rowCount(t, db, "st_user"); n != 1 {
		t.Errorf("数据应保持不变，期望 1 行，实际 %d 行", n)
	}
}

// 复制表迁移依赖整型 id，表结构不满足时必须在建影子表之前就失败，
// 不能等到切换工作表时才暴露
func TestMigrationStartRequiresIDColumn(t *testing.T) {
	cases := []struct {
		name    string
		columns string
	}{
		{"没有 id 列", "uid INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)"},
		{"id 不是整型", "id VARCHAR(36) NOT NULL PRIMARY KEY, name VARCHAR(50)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newTestDB(t)
			createTable(t, db, "st_user", c.columns)
			if err := db.Exec("INSERT INTO st_user (name) VALUES ('a')").Error; err != nil {
				// id 为 varchar 时需要显式给值
				if err := db.Exec("INSERT INTO st_user (id, name) VALUES ('x1','a')").Error; err != nil {
					t.Fatalf("准备数据失败：%v", err)
				}
			}

			m := NewMigration(db, "st_user", nil, nil, nil,
				"alter table st_user modify column name varchar(10)")
			err := m.Start()
			if err == nil {
				t.Fatal("表结构不满足要求时期望 Start 返回错误")
			}
			if !strings.Contains(err.Error(), "integer id") {
				t.Errorf("错误信息应说明 id 列的要求，实际：%v", err)
			}

			// 不应留下任何中间产物
			var logs int64
			db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Count(&logs)
			if logs != 0 {
				t.Errorf("失败时不应写入迁移记录，实际 %d 条", logs)
			}
			var tables []string
			db.Raw("SHOW TABLES LIKE 'st_user%'").Scan(&tables)
			if len(tables) != 1 {
				t.Errorf("失败时不应残留影子表，实际存在：%v", tables)
			}
			if n := rowCount(t, db, "st_user"); n != 1 {
				t.Errorf("原表数据应保持不变，期望 1 行，实际 %d 行", n)
			}
		})
	}
}

// 空表迁移是合法路径：取最大 id 会返回 ErrRecordNotFound，
// EndID 为 0 即是正确结果，不能被当成错误
func TestMigrationStartEmptyTable(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")

	m := NewMigration(db, "st_user", nil, nil, nil,
		"alter table st_user modify column name varchar(10)")
	if err := m.Start(); err != nil {
		t.Fatalf("空表迁移不应报错：%v", err)
	}

	var rec migrationLog
	if err := db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").
		Order("id desc").Take(&rec).Error; err != nil {
		t.Fatalf("未产生迁移记录：%v", err)
	}
	if rec.EndID != 0 || rec.TotalRecords != 0 {
		t.Errorf("空表期望 EndID=0 TotalRecords=0，实际 EndID=%d TotalRecords=%d", rec.EndID, rec.TotalRecords)
	}
	waitFor(t, 30*time.Second, "空表迁移完成", func() bool {
		var r migrationLog
		db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
		return r.Status == migrationStatus_completed
	})
	var ddl map[string]interface{}
	db.Raw("SHOW CREATE TABLE st_user").Scan(&ddl)
	if got := fmt.Sprint(ddl["Create Table"]); !strings.Contains(got, "varchar(10)") {
		t.Errorf("切换后的表结构未生效：\n%s", got)
	}
}

// 原表有数据而影子表为空时，必须拒绝切换工作表，
// 否则线上表会在一次 RENAME 后瞬间变空
func TestMigrationRefuseSwitchToEmptyShadow(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	createTable(t, db, "st_user_ghost", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	rows := []stUser{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}

	// 构造一条 EndID 为 0 的迁移记录，等价于取最大 id 失败后的状态：
	// 回填首轮就会判定“迁移完成”并尝试切换，而影子表还是空的
	markMigrating(t, db, "st_user", "st_user_ghost")
	var rec migrationLog
	db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Order("id desc").Take(&rec)

	// 用一条能走 online ddl 的语句启动，避免再生成新的迁移记录，
	// 让回填协程直接消费上面构造的这条
	m := NewMigration(db, "st_user", nil, nil, nil, "alter table st_user add column age int")
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	time.Sleep(500 * time.Millisecond) // 给回填协程足够的时间尝试切换

	// 工作表不能被切换
	if n := rowCount(t, db, "st_user"); n != 3 {
		t.Errorf("原表数据应保持不变，期望 3 行，实际 %d 行", n)
	}
	var r migrationLog
	db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
	if r.Status == migrationStatus_completed {
		t.Error("影子表为空时不应把迁移标记为已完成")
	}
	if r.OldTableBackupName != "" && db.Migrator().HasTable(r.OldTableBackupName) {
		t.Errorf("不应发生工作表切换，但出现了备份表 %s", r.OldTableBackupName)
	}
}

// 只有 MySQL 明确报出"不支持该 online ddl 算法"(1845/1846)时才回退到复制表迁移。
// 语法错误、列不存在这类问题必须直接报错，否则一处笔误会静默触发整张表的复制
func TestMigrationStartOnlyFallbackOnUnsupported(t *testing.T) {
	cases := []struct {
		name         string
		alterSQL     string
		wantFallback bool
	}{
		// 缩短 varchar 必须 COPY，MySQL 返回 1846
		{"缩短varchar需要COPY", "alter table st_user modify column name varchar(10)", true},
		// 改主键 + 加分区，MySQL 返回 1845
		{"改主键加分区", "alter table st_user drop primary key, add primary key(id,created_at) " +
			"PARTITION BY RANGE (created_at) (PARTITION p1 VALUES LESS THAN (100))", true},
		// 下面这些都不该回退
		{"语法错误", "alter table st_user modifyyy column name varchar(10)", false},
		{"列不存在", "alter table st_user modify column not_exist varchar(10)", false},
		{"表不存在", "alter table st_user_not_exist add column age int", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newTestDB(t)
			createTable(t, db, "st_user",
				"id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50), created_at BIGINT NOT NULL DEFAULT 0")
			if err := db.Create(&stUser{Name: "a"}).Error; err != nil {
				t.Fatalf("准备数据失败：%v", err)
			}

			m := NewMigration(db, "st_user", nil, nil, nil, c.alterSQL)
			err := m.Start()

			var logs int64
			db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Count(&logs)
			if c.wantFallback {
				if err != nil {
					t.Fatalf("应回退到复制表迁移，实际报错：%v", err)
				}
				if logs != 1 {
					t.Errorf("应产生 1 条迁移记录，实际 %d 条", logs)
				}
				return
			}
			if err == nil {
				t.Fatal("非 online ddl 相关的错误应直接返回，实际返回 nil")
			}
			if logs != 0 {
				t.Errorf("不应回退到复制表迁移，但产生了 %d 条迁移记录", logs)
			}
			var tables []string
			db.Raw("SHOW TABLES LIKE 'st_user\\_%'").Scan(&tables)
			if len(tables) != 0 {
				t.Errorf("不应产生影子表，实际：%v", tables)
			}
		})
	}
}

// 带 PARTITION BY 的语句要按“是否同时包含列变更”决定 ALGORITHM 子句插在哪里。
// 这个判断是按关键字做子串匹配的，关键字必须带上分隔空格：
// 否则 address / add_time 这类列名会命中 "add"，被误判成含列变更，
// 把 ALGORITHM 子句插到表名后面而拼出 `alter table t ,ALGORITHM=...`，
// MySQL 直接报 1064 语法错误，而 1064 不属于可回退的错误，整张表就迁不动了
func TestMigrationStartPartitionKeywordMatch(t *testing.T) {
	cases := []struct {
		name     string
		columns  string
		alterSQL string
	}{
		{
			name:    "分区键列名以 add 开头且不含列变更",
			columns: "id INT NOT NULL AUTO_INCREMENT, address VARCHAR(50) NOT NULL DEFAULT '', PRIMARY KEY(id,address)",
			alterSQL: "alter table st_user partition by range columns(address) " +
				"(PARTITION p1 VALUES LESS THAN ('m'), PARTITION p2 VALUES LESS THAN (MAXVALUE))",
		},
		{
			name:    "同时包含列变更与分区",
			columns: "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, created_at BIGINT NOT NULL DEFAULT 0",
			alterSQL: "alter table st_user drop primary key, add primary key(id,created_at) " +
				"PARTITION BY RANGE (created_at) (PARTITION p1 VALUES LESS THAN (100))",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newTestDB(t)
			createTable(t, db, "st_user", c.columns)
			m := NewMigration(db, "st_user", nil, nil, nil, c.alterSQL)
			if err := m.Start(); err != nil {
				t.Fatalf("应回退到复制表迁移，实际报错：%v", err)
			}
			var logs int64
			db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Count(&logs)
			if logs != 1 {
				t.Errorf("应产生 1 条迁移记录，实际 %d 条", logs)
			}
		})
	}
}

// 快照取到的 EndID 那一行在回填开始前被删除时，游标无法推进到 EndID。
// 若以 StartID == EndID 作为完成条件，回填会以 100ms 为周期永久空转
func TestMigrationBackfillMaxIDDeleted(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	rows := make([]stUser, 0, 300)
	for i := 0; i < 300; i++ {
		rows = append(rows, stUser{Name: fmt.Sprintf("u%d", i)})
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}
	maxID := rows[len(rows)-1].ID

	// 手工登记一条迁移记录，EndID 指向马上要被删除的那一行
	createTable(t, db, "st_user_ghost", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	if err := db.Migrator().AutoMigrate(&migrationLog{}); err != nil {
		t.Fatal(err)
	}
	rec := migrationLog{
		OldTableName: "st_user", NewTableName: "st_user_ghost",
		Status:  migrationStatus_in_progress,
		StartID: 0, EndID: int64(maxID), TotalRecords: 300,
	}
	if err := db.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	// 删掉 EndID 那一行，游标永远追不上 EndID
	if err := db.Exec("DELETE FROM st_user WHERE id = ?", maxID).Error; err != nil {
		t.Fatal(err)
	}

	m := NewMigration(db, "st_user", nil, nil, nil, "alter table st_user add index idx_name (name)")
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	// 回填必须能够收敛，而不是无限空转
	waitFor(t, 30*time.Second, "回填在最大 id 缺失时仍能完成", func() bool {
		var r migrationLog
		db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
		return r.Status == migrationStatus_completed
	})
	// 剩余 299 行应全部回填并完成切换
	if n := rowCount(t, db, "st_user"); n != 299 {
		t.Errorf("切换后数据条数期望 299，实际 %d", n)
	}
}

// 回填期间并发更新，切换后不能出现陈旧值
//
// 危险时序是：回填读到旧值 -> 业务更新原表并双写影子表（此时该行还没插入，影响 0 行）
// -> 回填把刚读到的旧值插入影子表。回填的 SELECT 加共享锁后，
// 业务事务持有的源行排他锁会挡住这次读取，读到的必然是新值
func TestMigrationConcurrentWriteDuringBackfill(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	const total = 1000
	rows := make([]stUser, 0, total)
	for i := 0; i < total; i++ {
		rows = append(rows, stUser{Name: fmt.Sprintf("old%d", i)})
	}
	if err := db.CreateInBatches(&rows, 200).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}

	// 缩短 varchar 必须走 COPY，触发复制表迁移
	m := NewMigration(db, "st_user", nil, nil, nil, "alter table st_user modify column name varchar(20)")
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	// 回填进行的同时逐行更新，让两者充分交错
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range rows {
			err := db.Model(&stUser{}).Where("id = ?", rows[i].ID).
				Update("name", fmt.Sprintf("new%d", i)).Error
			if err != nil {
				t.Errorf("并发更新失败：%v", err)
				return
			}
		}
	}()
	wg.Wait()

	var rec migrationLog
	db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Order("id desc").Take(&rec)
	waitFor(t, 60*time.Second, "并发写入下迁移完成", func() bool {
		var r migrationLog
		db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
		return r.Status == migrationStatus_completed
	})

	// 切换后的表里不允许残留任何 old 前缀的值
	if n := rowCount(t, db, "st_user"); n != total {
		t.Errorf("切换后数据条数期望 %d，实际 %d", total, n)
	}
	var stale int64
	db.Table("st_user").Where("name LIKE 'old%'").Count(&stale)
	if stale != 0 {
		var samples []map[string]interface{}
		db.Table("st_user").Where("name LIKE 'old%'").Limit(5).Find(&samples)
		t.Errorf("切换后仍有 %d 行是迁移前的陈旧值，样例：%v", stale, samples)
	}
	// 注意：不能拿备份表和线上表逐行比对——切换之后业务仍在继续更新线上表，
	// 备份表停在切换那一刻，两者本就应当不同
}

// 多实例并发 Start 时只能有一个真正进入准备阶段
//
// 这里不能靠事务互斥：MySQL 的 DDL 会隐式提交，事务和 SELECT ... FOR UPDATE
// 的锁在第一条 CREATE TABLE 处就失效了。若互斥失效会产生两条迁移记录和两张影子表，
// 双写只会写进后一张，而两个回填协程会各做一次 RENAME，先切上去的表又被换掉
func TestMigrationConcurrentStart(t *testing.T) {
	// 错开的毫秒数：影子表名里带 UnixMilli 时间戳，
	// 同一毫秒内两个实例会算出同名表而被“建表重名”意外挡住，
	// 覆盖不到真正的互斥逻辑
	for _, stagger := range []time.Duration{0, 5 * time.Millisecond} {
		t.Run(fmt.Sprintf("错开%v", stagger), func(t *testing.T) {
			db := newTestDB(t)
			createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
			rows := make([]stUser, 0, 3000)
			for i := 0; i < 3000; i++ {
				rows = append(rows, stUser{Name: fmt.Sprintf("u%d", i)})
			}
			if err := db.CreateInBatches(&rows, 500).Error; err != nil {
				t.Fatalf("准备数据失败：%v", err)
			}

			const instances = 4
			alter := "alter table st_user modify column name varchar(10)"
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < instances; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					m := NewMigration(db, "st_user", nil, nil, nil, alter)
					<-start
					time.Sleep(time.Duration(i) * stagger)
					if err := m.Start(); err != nil {
						t.Errorf("实例%d Start 返回错误：%v", i, err)
					}
				}(i)
			}
			close(start)
			wg.Wait()

			var logs int64
			db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Count(&logs)
			if logs != 1 {
				t.Errorf("期望只产生 1 条迁移记录，实际 %d 条", logs)
			}
			var tables []string
			db.Raw("SHOW TABLES LIKE 'st_user\\_%'").Scan(&tables)
			if len(tables) != 1 {
				t.Errorf("期望只产生 1 张影子表，实际 %d 张：%v", len(tables), tables)
			}

			// 迁移仍应正常完成，数据一条不少
			var rec migrationLog
			db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").Order("id desc").Take(&rec)
			waitFor(t, 60*time.Second, "并发场景下迁移完成", func() bool {
				var r migrationLog
				db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
				return r.Status == migrationStatus_completed
			})
			if n := rowCount(t, db, "st_user"); n != 3000 {
				t.Errorf("切换后数据条数期望 3000，实际 %d", n)
			}
		})
	}
}

// migrationParams 会被 Hook 在每次 DML 时并发读取，
// 同时 NewMigration 可能在任意时刻写入。无保护的 map 并发读写不是"偶尔出错"，
// 而是 runtime 直接抛 concurrent map read and map write 终止进程。
// 该用例需配合 -race 运行
func TestMigrationParamsConcurrentAccess(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "dw_user", dwColumns)
	createTable(t, db, "dw_user_ghost", dwColumns)
	markMigrating(t, db, "dw_user", "dw_user_ghost")
	NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")

	done := make(chan struct{})
	var writer, readers sync.WaitGroup
	// 写入方：不断重新注册，直到读取方全部结束
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-done:
				return
			default:
				NewMigration(db, "dw_user", nil, nil, nil, "alter table dw_user add column c1 int")
			}
		}
	}()
	// 读取方：并发写库，触发 Hook 读 map
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func(i int) {
			defer readers.Done()
			for j := 0; j < 25; j++ {
				if err := db.Create(&dwUser{Name: fmt.Sprintf("u%d-%d", i, j)}).Error; err != nil {
					t.Errorf("并发写入失败：%v", err)
					return
				}
			}
		}(i)
	}
	readers.Wait()
	close(done)
	writer.Wait()

	if o, g := rowCount(t, db, "dw_user"), rowCount(t, db, "dw_user_ghost"); o != 100 || g != 100 {
		t.Errorf("期望两表各 100 行，实际 原表=%d 影子=%d", o, g)
	}
}

// 迁移锁必须在准备阶段结束后释放，否则后续迁移会被永久挡住
func TestMigrationLockReleased(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")
	m := NewMigration(db, "st_user", nil, nil, nil, "alter table st_user modify column name varchar(10)")
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	// IS_FREE_LOCK 返回 1 表示锁已释放
	var free sql.NullInt64
	if err := db.Raw("SELECT IS_FREE_LOCK(?)", m.migrateLockName()).Scan(&free).Error; err != nil {
		t.Fatalf("查询锁状态失败：%v", err)
	}
	if !free.Valid || free.Int64 != 1 {
		t.Errorf("准备阶段结束后迁移锁应已释放，实际 IS_FREE_LOCK=%v", free)
	}
}

// 锁名超长时要退化成哈希，否则 MySQL 会拒绝（用户锁名上限 64）
func TestMigrationLockName(t *testing.T) {
	short := (&Migration{TableName: "st_user"}).migrateLockName()
	if short != migrateLockPrefix+"st_user" {
		t.Errorf("短表名应直接拼接，实际 %q", short)
	}
	long := (&Migration{TableName: strings.Repeat("t", 64)}).migrateLockName()
	if len(long) > migrateLockNameMaxLen {
		t.Errorf("锁名长度不能超过 %d，实际 %d：%q", migrateLockNameMaxLen, len(long), long)
	}
	// 同一张表必须稳定映射到同一个锁名
	if long != (&Migration{TableName: strings.Repeat("t", 64)}).migrateLockName() {
		t.Error("同一表名两次生成的锁名不一致")
	}
	if long == (&Migration{TableName: strings.Repeat("x", 64)}).migrateLockName() {
		t.Error("不同表名生成了相同的锁名")
	}
}

// 不支持 online ddl 时回退到复制表迁移：
// 建影子表 -> 双写 -> 回填历史数据 -> 原子切换工作表
// 缩短 varchar 长度在 MySQL 中必须走 COPY，因此一定会触发回退
func TestMigrationStartCopyFallback(t *testing.T) {
	db := newTestDB(t)
	createTable(t, db, "st_user", "id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50)")

	const total = 500 // 大于单批 200 条，覆盖多批回填
	rows := make([]stUser, 0, total)
	for i := 0; i < total; i++ {
		rows = append(rows, stUser{Name: fmt.Sprintf("u%d", i)})
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}

	m := NewMigration(db, "st_user", nil, nil, nil,
		"alter table st_user modify column name varchar(10)")
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	// 确认确实走了回退分支
	var rec migrationLog
	if err := db.Model(&migrationLog{}).Where("old_table_name = ?", "st_user").
		Order("id desc").Take(&rec).Error; err != nil {
		t.Fatalf("未产生迁移记录，说明没有走复制表流程：%v", err)
	}
	if rec.TotalRecords != total {
		t.Errorf("登记的总条数期望 %d，实际 %d", total, rec.TotalRecords)
	}

	waitFor(t, 30*time.Second, "回填完成并切换工作表", func() bool {
		var r migrationLog
		db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
		return r.Status == migrationStatus_completed
	})

	// 切换后 st_user 应当是改造过的新表，数据一条不少
	if n := rowCount(t, db, "st_user"); n != total {
		t.Errorf("切换后数据条数期望 %d，实际 %d", total, n)
	}
	var ddl map[string]interface{}
	if err := db.Raw("SHOW CREATE TABLE st_user").Scan(&ddl).Error; err != nil {
		t.Fatalf("读取表结构失败：%v", err)
	}
	if got := fmt.Sprint(ddl["Create Table"]); !strings.Contains(got, "varchar(10)") {
		t.Errorf("切换后的表结构未生效，仍未看到 varchar(10)：\n%s", got)
	}

	// 原表被重命名为备份表保留
	var backup migrationLog
	db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&backup)
	if backup.OldTableBackupName == "" {
		t.Error("未记录旧表备份名称")
	} else if !db.Migrator().HasTable(backup.OldTableBackupName) {
		t.Errorf("旧表备份 %s 不存在", backup.OldTableBackupName)
	} else if n := rowCount(t, db, backup.OldTableBackupName); n != total {
		t.Errorf("备份表数据条数期望 %d，实际 %d", total, n)
	}

	// 迁移结束后双写应自动停止，不再有额外开销与副作用
	if err := db.Create(&stUser{Name: "after"}).Error; err != nil {
		t.Fatalf("切换后写入失败：%v", err)
	}
	if n := rowCount(t, db, "st_user"); n != total+1 {
		t.Errorf("切换后写入异常，期望 %d 行，实际 %d 行", total+1, n)
	}
	if n := rowCount(t, db, backup.OldTableBackupName); n != total {
		t.Errorf("迁移已完成，不应再往备份表双写，期望 %d 行，实际 %d 行", total, n)
	}
}
