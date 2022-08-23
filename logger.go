package gormx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	glogger "gorm.io/gorm/logger"
	"gorm.io/gorm/utils"

	"github.com/itmisx/logger"
)

var LocalDebug bool

// Colors
const (
	Reset       = "\033[0m"
	Red         = "\033[31m"
	Green       = "\033[32m"
	Yellow      = "\033[33m"
	Blue        = "\033[34m"
	Magenta     = "\033[35m"
	Cyan        = "\033[36m"
	White       = "\033[37m"
	BlueBold    = "\033[34;1m"
	MagentaBold = "\033[35;1m"
	RedBold     = "\033[31;1m"
	YellowBold  = "\033[33;1m"
)

type LogLevel int

func init() {
	_, err := os.Stat("./__debug_bin")
	if err == nil {
		LocalDebug = true
	}
}

type mylogger struct {
	glogger.Writer
	glogger.Config
	infoStr, warnStr, errStr            string
	traceStr, traceErrStr, traceWarnStr string
}

func NewLogger(writer glogger.Writer, config glogger.Config) glogger.Interface {
	var (
		infoStr      = "%s\n[info] "
		warnStr      = "%s\n[warn] "
		errStr       = "%s\n[error] "
		traceStr     = "%s\n[%.3fms] [rows:%v] %s"
		traceWarnStr = "%s %s\n[%.3fms] [rows:%v] %s"
		traceErrStr  = "%s %s\n[%.3fms] [rows:%v] %s"
	)

	if config.Colorful {
		infoStr = Green + "%s\n" + Reset + Green + "[info] " + Reset
		warnStr = BlueBold + "%s\n" + Reset + Magenta + "[warn] " + Reset
		errStr = Magenta + "%s\n" + Reset + Red + "[error] " + Reset
		traceStr = Green + "%s\n" + Reset + Yellow + "[%.3fms] " + BlueBold + "[rows:%v]" + Reset + " %s"
		traceWarnStr = Green + "%s " + Yellow + "%s\n" + Reset + RedBold + "[%.3fms] " + Yellow + "[rows:%v]" + Magenta + " %s" + Reset
		traceErrStr = RedBold + "%s " + MagentaBold + "%s\n" + Reset + Yellow + "[%.3fms] " + BlueBold + "[rows:%v]" + Reset + " %s"
	}
	return &mylogger{
		Writer:       writer,
		Config:       config,
		infoStr:      infoStr,
		warnStr:      warnStr,
		errStr:       errStr,
		traceStr:     traceStr,
		traceWarnStr: traceWarnStr,
		traceErrStr:  traceErrStr,
	}
}

// LogMode log mode
func (l *mylogger) LogMode(level glogger.LogLevel) glogger.Interface {
	newlogger := *l
	newlogger.LogLevel = level
	return &newlogger
}

// Info print info
func (l mylogger) Info(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= glogger.Info {
		if LocalDebug {
			l.Printf(l.infoStr+msg, append([]interface{}{utils.FileWithLineNum()}, data...)...)
		} else {
			var strs []string
			for _, s := range data {
				if str, ok := s.(string); ok {
					strs = append(strs, str)
				}
			}
			logger.Info(ctx, msg, logger.String("line", utils.FileWithLineNum()), logger.StringSlice("detail", strs))
		}
	}
}

// Warn print warn messages
func (l mylogger) Warn(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= glogger.Warn {
		if LocalDebug {
			l.Printf(l.warnStr+msg, append([]interface{}{utils.FileWithLineNum()}, data...)...)
		} else {
			var strs []string
			for _, s := range data {
				if str, ok := s.(string); ok {
					strs = append(strs, str)
				}
			}
			logger.Info(ctx, msg, logger.String("line", utils.FileWithLineNum()), logger.StringSlice("detail", strs))
		}
	}
}

// Error print error messages
func (l mylogger) Error(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= glogger.Error {
		if LocalDebug {
			l.Printf(l.errStr+msg, append([]interface{}{utils.FileWithLineNum()}, data...)...)
		} else {
			var strs []string
			for _, s := range data {
				if str, ok := s.(string); ok {
					strs = append(strs, str)
				}
			}
			logger.Info(ctx, msg, logger.String("line", utils.FileWithLineNum()), logger.StringSlice("detail", strs))
		}
	}
}

// Trace print sql message
func (l mylogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.LogLevel <= glogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	switch {
	case err != nil && l.LogLevel >= glogger.Error && (!errors.Is(err, glogger.ErrRecordNotFound) || !l.IgnoreRecordNotFoundError):
		sql, rows := fc()
		sql = removeEscapeCharacter(sql)
		if rows == -1 {
			if LocalDebug {
				l.Printf(l.traceErrStr, utils.FileWithLineNum(), err, float64(elapsed.Nanoseconds())/1e6, "-", sql)
			} else {
				logger.Error(ctx,
					"sql error",
					logger.String("err", err.Error()),
					logger.String("line", utils.FileWithLineNum()),
					logger.Float64("elapsed[ms]", float64(elapsed.Nanoseconds())/1e6),
					logger.String("rows affected", "-"),
					logger.String("sql", sql))
			}
		} else {
			if LocalDebug {
				l.Printf(l.traceErrStr, utils.FileWithLineNum(), err, float64(elapsed.Nanoseconds())/1e6, rows, sql)
			} else {
				logger.Error(ctx,
					"sql error",
					logger.String("err", err.Error()),
					logger.String("line", utils.FileWithLineNum()),
					logger.Float64("elapsed[ms]", float64(elapsed.Nanoseconds())/1e6),
					logger.Int64("rows affected", rows),
					logger.String("sql", sql))
			}
		}
	case elapsed > l.SlowThreshold && l.SlowThreshold != 0 && l.LogLevel >= glogger.Warn:
		sql, rows := fc()
		sql = removeEscapeCharacter(sql)
		slowLog := fmt.Sprintf("SLOW SQL >= %v", l.SlowThreshold)
		if rows == -1 {
			if LocalDebug {
				l.Printf(l.traceWarnStr, utils.FileWithLineNum(), slowLog, float64(elapsed.Nanoseconds())/1e6, "-", sql)
			} else {
				logger.Warn(ctx,
					"sql warn",
					logger.String("warn", slowLog),
					logger.String("line", utils.FileWithLineNum()),
					logger.Float64("elapsed[ms]", float64(elapsed.Nanoseconds())/1e6),
					logger.String("rows affected", "-"),
					logger.String("sql", sql))
			}
		} else {
			if LocalDebug {
				l.Printf(l.traceWarnStr, utils.FileWithLineNum(), slowLog, float64(elapsed.Nanoseconds())/1e6, rows, sql)
			} else {
				logger.Warn(ctx,
					"sql warn",
					logger.String("warn", slowLog),
					logger.String("line", utils.FileWithLineNum()),
					logger.Float64("elapsed[ms]", float64(elapsed.Nanoseconds())/1e6),
					logger.Int64("rows affected", rows),
					logger.String("sql", sql))
			}
		}
	case l.LogLevel == glogger.Info:
		sql, rows := fc()
		sql = removeEscapeCharacter(sql)
		if rows == -1 {
			if LocalDebug {
				l.Printf(l.traceStr, utils.FileWithLineNum(), float64(elapsed.Nanoseconds())/1e6, "-", sql)
			} else {
				logger.Info(ctx,
					"sql info",
					logger.String("line", utils.FileWithLineNum()),
					logger.Float64("elapsed[ms]", float64(elapsed.Nanoseconds())/1e6),
					logger.String("rows affected", "-"),
					logger.String("sql", sql))
			}
		} else {
			if LocalDebug {
				l.Printf(l.traceStr, utils.FileWithLineNum(), float64(elapsed.Nanoseconds())/1e6, rows, sql)
			} else {
				logger.Info(ctx,
					"sql info",
					logger.String("line", utils.FileWithLineNum()),
					logger.Float64("elapsed[ms]", float64(elapsed.Nanoseconds())/1e6),
					logger.Int64("rows affected", rows),
					logger.String("sql", sql))
			}
		}
	}
}

// 去除转义字符
func removeEscapeCharacter(sql string) string {
	// remove \r
	sql = strings.ReplaceAll(sql, "\r", "")
	// remove \n
	sql = strings.ReplaceAll(sql, "\n", "")
	// remove \t
	sql = strings.ReplaceAll(sql, "\t", "")
	return sql
}
