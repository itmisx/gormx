package gormx

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

// 这里的用例都通过 gormx.New 建立连接，覆盖的是「带 dbresolver 的真实链路」，
// 与 migration_test.go 里直连 gorm.Open 的用例互补：
// dbresolver 会在每条语句执行前改写 Statement.ConnPool，
// 迁移锁、双写这些逻辑在它下面能否正常工作，只有走这条路径才测得到。

// newGormxDB 建一个干净的独立库，并通过 gormx.New 连接（含一个从库地址，激活 dbresolver）
func newGormxDB(t *testing.T) *gorm.DB {
	t.Helper()
	// 先用 newTestDB 确认 MySQL 可用并准备好独立库。
	// 这一步不能省：New() 连接失败时会无限重试，MySQL 不可用时会一直阻塞
	newTestDB(t)

	db, err := New(Config{
		Username: "root",
		Password: "123456",
		Addrs:    []string{"127.0.0.1:13306", "127.0.0.1:13306"}, // [0] 主库，其余从库
		Database: testDatabase,
	})
	if err != nil {
		t.Fatalf("gormx.New 失败：%v", err)
	}
	limitPool(t, db)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return db
}

// 迁移测试用 model
type migrationTest struct {
	ID        int    `json:"id" gorm:"column:id;type:int(8);primaryKey;autoIncrement"`
	Name      string `json:"name" gorm:"column:name;type:varchar(20)"`
	CreatedAt int64  `json:"created_at" gorm:"column:created_at;type:bigint;autoCreateTime;default:0"`
	Migration `gorm:"-"`
}

func (migrationTest) TableName() string {
	return "migration_test"
}

// New() 的冒烟测试：连接可用、读写正常
func TestGormx(t *testing.T) {
	db := newGormxDB(t)

	if err := db.Migrator().AutoMigrate(&migrationTest{}); err != nil {
		t.Fatalf("建表失败：%v", err)
	}
	row := &migrationTest{Name: "a"}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("插入失败：%v", err)
	}
	if row.ID == 0 {
		t.Error("插入后未回填自增主键")
	}

	var got migrationTest
	if err := db.Where("id = ?", row.ID).Take(&got).Error; err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if got.Name != "a" {
		t.Errorf("查询结果不符，期望 name=a，实际 %q", got.Name)
	}

	if err := db.Model(&migrationTest{}).Where("id = ?", row.ID).
		Update("name", "b").Error; err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	db.Where("id = ?", row.ID).Take(&got)
	if got.Name != "b" {
		t.Errorf("更新未生效，实际 name=%q", got.Name)
	}
}

// 迁移期间持续写入：验证 dbresolver 链路下双写与切换都正常
func TestMigrate(t *testing.T) {
	db := newGormxDB(t)
	if err := db.Migrator().AutoMigrate(&migrationTest{}); err != nil {
		t.Fatalf("建表失败：%v", err)
	}

	const preset = 500
	rows := make([]migrationTest, 0, preset)
	for i := 0; i < preset; i++ {
		rows = append(rows, migrationTest{Name: strconv.Itoa(i)})
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatalf("准备数据失败：%v", err)
	}

	// 缩短 varchar 必须走 COPY，一定会回退到复制表迁移
	m := NewMigration(db, "migration_test", nil, nil, nil,
		"alter table migration_test modify column name varchar(10)")
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	// 迁移进行的同时持续写入
	const extra = 200
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < extra; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := db.Create(&migrationTest{Name: "n" + strconv.Itoa(i)}).Error; err != nil {
				t.Errorf("迁移期间写入失败：%v", err)
				return
			}
		}
	}()
	wg.Wait()
	close(stop)

	var rec migrationLog
	if err := db.Model(&migrationLog{}).Where("old_table_name = ?", "migration_test").
		Order("id desc").Take(&rec).Error; err != nil {
		t.Fatalf("未产生迁移记录：%v", err)
	}
	waitFor(t, 60*time.Second, "dbresolver 链路下迁移完成", func() bool {
		var r migrationLog
		db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
		return r.Status == migrationStatus_completed
	})

	// 切换后：结构已变更，数据一条不少
	if n := rowCount(t, db, "migration_test"); n != preset+extra {
		t.Errorf("切换后数据条数期望 %d，实际 %d", preset+extra, n)
	}
	var ddl map[string]interface{}
	if err := db.Raw("SHOW CREATE TABLE migration_test").Scan(&ddl).Error; err != nil {
		t.Fatalf("读取表结构失败：%v", err)
	}
	if got := fmt.Sprint(ddl["Create Table"]); !strings.Contains(strings.ToLower(got), "varchar(10)") {
		t.Errorf("表结构未生效，仍未看到 varchar(10)：\n%s", got)
	}
}

// 迁移到分区表：验证切换后分区真的建立起来了
func TestMigratePartition(t *testing.T) {
	db := newGormxDB(t)
	if err := db.Migrator().AutoMigrate(&migrationTest{}); err != nil {
		t.Fatalf("建表失败：%v", err)
	}
	for i := 0; i < 50; i++ {
		if err := db.Create(&migrationTest{Name: strconv.Itoa(i)}).Error; err != nil {
			t.Fatalf("准备数据失败：%v", err)
		}
	}

	m := NewMigration(db, "migration_test", nil, nil, nil,
		`alter table migration_test
		drop primary key,
		add primary key(id,created_at)
		PARTITION BY RANGE (created_at) (
		  PARTITION p20250101 VALUES LESS THAN (UNIX_TIMESTAMP('2025-01-01')),
		  PARTITION p20260101 VALUES LESS THAN (UNIX_TIMESTAMP('2026-01-01')),
		  PARTITION p20270101 VALUES LESS THAN (UNIX_TIMESTAMP('2027-01-01'))
		)`,
	)
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	var rec migrationLog
	if err := db.Model(&migrationLog{}).Where("old_table_name = ?", "migration_test").
		Order("id desc").Take(&rec).Error; err != nil {
		t.Fatalf("未产生迁移记录：%v", err)
	}
	waitFor(t, 60*time.Second, "分区改造完成", func() bool {
		var r migrationLog
		db.Model(&migrationLog{}).Where("id = ?", rec.ID).Take(&r)
		return r.Status == migrationStatus_completed
	})

	if n := rowCount(t, db, "migration_test"); n != 50 {
		t.Errorf("切换后数据条数期望 50，实际 %d", n)
	}
	var parts []string
	db.Table("information_schema.PARTITIONS").
		Where("TABLE_SCHEMA = ?", testDatabase).
		Where("TABLE_NAME = ?", "migration_test").
		Where("PARTITION_NAME IS NOT NULL").
		Pluck("PARTITION_NAME", &parts)
	if len(parts) != 3 {
		t.Errorf("切换后期望 3 个分区，实际 %d 个：%v", len(parts), parts)
	}
}

// ---------------------------------------------------------------- 版本管理

// 版本控制器的语义（由 Upgrade 的实现决定）：
//   - 库中没有任何版本记录时执行 InstallFunc，随后把当前版本直接置为最高版本，
//     因此 InstallFunc 必须建出「当前版本的完整结构」，中间各版本的升级函数不会执行
//   - 循环条件是 ver < maxVersion 才跳过，所以与当前版本相同的那个升级函数
//     每次 Upgrade 都会重新执行一遍 —— 非幂等语句必须用 MigrateOnce 包起来

type versionUpgrade struct{ db *gorm.DB }

var versionCalls []string

func (u versionUpgrade) V1() error {
	versionCalls = append(versionCalls, "V1")
	return MigrateOnce(u.db, "add_age", func() error {
		versionCalls = append(versionCalls, "V1.exec")
		return u.db.Exec("alter table version_demo add column age int").Error
	})
}

func (u versionUpgrade) V2() error {
	versionCalls = append(versionCalls, "V2")
	return MigrateOnce(u.db, "backfill_age", func() error {
		versionCalls = append(versionCalls, "V2.exec")
		return u.db.Exec("update version_demo set age = 18").Error
	})
}

func TestVersion(t *testing.T) {
	db := newGormxDB(t)
	versionCalls = nil
	installed := 0
	install := func() {
		installed++
		// 全新安装直接建出当前版本的完整结构
		db.Exec("CREATE TABLE version_demo (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, " +
			"name VARCHAR(50), age INT)")
		db.Exec("INSERT INTO version_demo (name) VALUES ('a')")
	}

	vc := NewVersionController(db, versionUpgrade{db: db}, install)
	if err := vc.Upgrade(); err != nil {
		t.Fatalf("首次 Upgrade 失败：%v", err)
	}
	if installed != 1 {
		t.Errorf("install 期望执行 1 次，实际 %d 次", installed)
	}
	// 全新安装时只会执行最高版本的升级函数
	if contains(versionCalls, "V1") {
		t.Errorf("全新安装不应执行 V1，实际调用序列：%v", versionCalls)
	}
	if !contains(versionCalls, "V2.exec") {
		t.Errorf("V2 未执行，实际调用序列：%v", versionCalls)
	}
	var age int
	db.Table("version_demo").Select("COALESCE(age,0)").Scan(&age)
	if age != 18 {
		t.Errorf("V2 的数据订正未生效，age 期望 18，实际 %d", age)
	}
	var logs int64
	db.Model(&VersionLog{}).Where("migration_name = ?", "backfill_age").Count(&logs)
	if logs != 1 {
		t.Errorf("backfill_age 的执行记录期望 1 条，实际 %d 条", logs)
	}

	// 二次升级：install 不再执行，MigrateOnce 包裹的语句也不再执行
	before := len(versionCalls)
	if err := vc.Upgrade(); err != nil {
		t.Fatalf("二次 Upgrade 不应返回错误：%#v", err)
	}
	if installed != 1 {
		t.Errorf("install 不应重复执行，实际累计 %d 次", installed)
	}
	for _, c := range versionCalls[before:] {
		if c == "V1.exec" || c == "V2.exec" {
			t.Errorf("MigrateOnce 包裹的语句重复执行了：%v", versionCalls[before:])
			break
		}
	}
}

func TestMigrateOnce(t *testing.T) {
	db := newGormxDB(t)
	if err := db.Migrator().AutoMigrate(&VersionLog{}); err != nil {
		t.Fatal(err)
	}

	t.Run("只执行一次且不返回错误", func(t *testing.T) {
		runs := 0
		for i := 0; i < 3; i++ {
			err := MigrateOnce(db, "once", func() error { runs++; return nil })
			// 已经执行过不是错误：若这里返回 error，升级函数会把它带回 Upgrade，
			// 导致第一次升级之后每次启动都失败
			if err != nil {
				t.Fatalf("第 %d 次调用返回错误：%v", i+1, err)
			}
		}
		if runs != 1 {
			t.Errorf("迁移函数期望执行 1 次，实际 %d 次", runs)
		}
		var logs int64
		db.Model(&VersionLog{}).Where("migration_name = ?", "once").Count(&logs)
		if logs != 1 {
			t.Errorf("执行记录期望 1 条，实际 %d 条", logs)
		}
	})

	// 迁移失败时不能留下执行记录，否则这段迁移永远不会被重试
	t.Run("失败后可重试", func(t *testing.T) {
		err := MigrateOnce(db, "retry", func() error { return fmt.Errorf("boom") })
		if err == nil {
			t.Fatal("迁移函数失败时期望返回错误")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("错误信息应包含底层原因，实际：%v", err)
		}
		var logs int64
		db.Model(&VersionLog{}).Where("migration_name = ?", "retry").Count(&logs)
		if logs != 0 {
			t.Fatalf("失败后不应留下执行记录，实际 %d 条", logs)
		}

		// 下一次启动应当能重新执行
		ran := false
		if err := MigrateOnce(db, "retry", func() error { ran = true; return nil }); err != nil {
			t.Fatalf("重试失败：%v", err)
		}
		if !ran {
			t.Error("失败之后应当可以重试，实际没有再执行")
		}
	})
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
