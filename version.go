package gormx

import (
	"errors"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

type versionController struct {
	DB            *gorm.DB // gorm Engine
	upgradeStruct interface{}
	InstallFunc   func()
}

type VersionLog struct {
	ID            string `json:"id" gorm:"column:id;type:int;size:64;primaryKey;autoIncrement"`
	Version       int64  `json:"version" gorm:"column:version;type:int;size:64;uniqueIndex:uk_migration,priority:1;comment:数据库版本"`
	MigrationName string `json:"migration_name" gorm:"column:migration_name;type:varchar(50);default:'';uniqueIndex:uk_migration,priority:2;comment:迁移名称"`
	CreatedAt     int64  `json:"created_at" gorm:"column:created_at;type:int;size:64;autoCreateTime;comment:创建时间"`
}

func (VersionLog) TableName() string {
	return "version_log"
}

// 实例化版本控制实例
func NewVersionController(
	db *gorm.DB,
	upgradeStruct interface{},
	installFunc func(),
) *versionController {
	db.Migrator().AutoMigrate(&VersionLog{})
	return &versionController{
		DB:            db,
		upgradeStruct: upgradeStruct,
		InstallFunc:   installFunc,
	}
}

// Upgrade 数据库版本升级
func (vc *versionController) Upgrade() error {
	versionList := []int{}
	var versionFuncMap = make(map[int]reflect.Method)
	// 获取MigrationStruct的所有方法
	upgradeStructVal := reflect.ValueOf(vc.upgradeStruct)
	t := reflect.TypeOf(vc.upgradeStruct)
	for i := 0; i < t.NumMethod(); i++ {
		method := t.Method(i)
		// 从方法名提取版本号
		methedVInt := 0
		reg, _ := regexp.Compile(`\d+`)
		numPart := reg.FindAllString(method.Name, -1)
		if len(numPart) > 0 {
			methedVInt, _ = strconv.Atoi(numPart[0])
		}
		// 放入版本列表
		versionList = append(versionList, methedVInt)
		// 版本号与升级方法映射
		versionFuncMap[methedVInt] = method
	}
	// 升序
	sort.Ints(versionList)
	// 读取versionLog,获取最新的版本
	var maxVersion VersionLog
	vc.DB.Model(&VersionLog{}).Select("max(version) as version").Take(&maxVersion)
	if maxVersion.Version == 0 && len(versionList) > 0 {
		vc.InstallFunc()
		maxVersion.Version = int64(versionList[len(versionList)-1])
	}
	// 对比versionLog，进行升级
	for _, ver := range versionList {
		if ver < int(maxVersion.Version) {
			continue
		}
		// 获取版本对应的函数
		if _, ok := versionFuncMap[ver]; ok {
			// 执行并获取执行的结果
			out := versionFuncMap[ver].Func.Call([]reflect.Value{upgradeStructVal})
			if len(out) > 0 {
				if rv, ok := out[0].Interface().(error); ok {
					if rv != nil {
						return rv
					}
				}
			}
			// 是否已经有版本记录
			var count int64
			vc.DB.Model(&VersionLog{}).Where("version = ?", ver).Count(&count)
			if count > 0 {
				continue
			}
			// 记录升级日志
			vc.DB.Model(&VersionLog{}).Create(&VersionLog{
				Version:       int64(ver),
				MigrationName: "",
			})
		}
	}
	return nil
}

// MigrateOnce 限制某些迁移语句只能执行一次
// 在升级函数中使用
func MigrateOnce(
	db *gorm.DB,
	migrationName string,
	migrationFunc func() error,
) error {
	// 获取调用的函数（含包名）
	pc, _, _, _ := runtime.Caller(1)
	fullName := runtime.FuncForPC(pc).Name()
	// 提取最后的函数名部分（去掉包路径）
	parts := strings.Split(fullName, ".")
	funcName := parts[len(parts)-1] // 函数名字
	// 从函数获取版本
	version := 0
	reg, _ := regexp.Compile(`\d+`)
	numPart := reg.FindAllString(funcName, -1)
	if len(numPart) > 0 {
		version, _ = strconv.Atoi(numPart[0])
	}

	// 判断是否执行过
	var count int64
	db.Model(&VersionLog{}).
		Where("version = ?", version).
		Where("migration_name = ?", migrationName).Count(&count)
	if count > 0 {
		return errors.New("")
	}
	// 插入执行记录
	return db.Transaction(func(tx *gorm.DB) error {
		rowsAffected := db.Model(&VersionLog{}).Create(&VersionLog{
			Version:       int64(version),
			MigrationName: migrationName,
		}).RowsAffected
		if rowsAffected < 1 {
			return errors.New("insert exec log failed")
		}
		if err := migrationFunc(); err != nil {
			return errors.New(err.Error())
		}
		return nil
	})
}
