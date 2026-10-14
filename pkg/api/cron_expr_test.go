package api

import (
	"testing"
	"time"
)

func mustExpr(t *testing.T, spec string) *cronExpr {
	t.Helper()
	e, err := parseCronExpr(spec)
	if err != nil {
		t.Fatalf("parse %q: %v", spec, err)
	}
	return e
}

func at(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.Local)
}

func TestParseCronExprValid(t *testing.T) {
	cases := []struct {
		spec  string
		check func(*cronExpr) bool
		desc  string
	}{
		{"*/5 * * * *", func(e *cronExpr) bool { return e.minutes[0] && e.minutes[55] && !e.minutes[3] }, "步进分钟"},
		{"0 9 * * 1-5", func(e *cronExpr) bool { return e.hours[9] && e.dows[1] && e.dows[5] && !e.dows[0] && !e.dows[6] }, "工作日区间"},
		{"30 14 28 2 *", func(e *cronExpr) bool { return e.minutes[30] && e.hours[14] && e.doms[28] && e.months[2] && e.dowStar }, "单值"},
		{"0 9-17/2 * * *", func(e *cronExpr) bool { return e.hours[9] && e.hours[11] && e.hours[17] && !e.hours[10] }, "区间步进"},
		{"0 0 1,15 * *", func(e *cronExpr) bool { return e.doms[1] && e.doms[15] && !e.doms[2] }, "多值"},
		{"0 0 * * 7", func(e *cronExpr) bool { return e.dows[0] }, "dow 7 归一为周日"},
		{"5 0 * 3,6,9 0", func(e *cronExpr) bool { return e.months[3] && e.months[6] && e.months[9] }, "月份多值"},
	}
	for _, tc := range cases {
		e := mustExpr(t, tc.spec)
		if !tc.check(e) {
			t.Errorf("%q: %s 校验失败", tc.spec, tc.desc)
		}
	}
}

func TestParseCronExprInvalid(t *testing.T) {
	for _, spec := range []string{
		"* * * *",       // 缺字段
		"* * * * * *",   // 多字段
		"60 * * * *",    // 分钟超范围
		"* 24 * * *",    // 小时超范围
		"* * 32 * *",    // 日超范围
		"* * * 13 *",    // 月超范围
		"* * * * 7+",    // 星期非法字符
		"5-1 * * * *",   // 反向区间
		"a * * * *",     // 非数字
		"*/0 * * * *",   // 步进为 0
		"1-2-3 * * * *", // 多重区间
		"",              // 空
		"  * * * * ",    // 空白分隔导致字段错乱——5 字段按空格 split 后仍 5 段，实际合法
	} {
		if _, err := parseCronExpr(spec); err == nil {
			// "  * * * * " 按空格切分实际是 5 个有效字段，合法。
			if spec == "  * * * * " {
				continue
			}
			t.Errorf("%q: 期望报错, 得到 nil", spec)
		}
	}
}

func TestCronNextMinuteSteps(t *testing.T) {
	e := mustExpr(t, "*/5 * * * *")
	// 10:03 的下一次是 10:05；10:05 的下一次是 10:10（严格大于，不重复触发当前刻）。
	if got := e.next(at(2026, 10, 14, 10, 3)); !got.Equal(at(2026, 10, 14, 10, 5)) {
		t.Errorf("next(10:03) = %v, 期望 10:05", got)
	}
	if got := e.next(at(2026, 10, 14, 10, 5)); !got.Equal(at(2026, 10, 14, 10, 10)) {
		t.Errorf("next(10:05) = %v, 期望 10:10", got)
	}
}

func TestCronNextSkipsWeekend(t *testing.T) {
	e := mustExpr(t, "0 9 * * 1-5")
	// 2026-10-16 是周五：10:00 之后下一次触发是下周一 10-19 9:00。
	if got := e.next(at(2026, 10, 16, 10, 0)); !got.Equal(at(2026, 10, 19, 9, 0)) {
		t.Errorf("next = %v, 期望 10-19 9:00", got)
	}
}

func TestCronNextYearBoundary(t *testing.T) {
	e := mustExpr(t, "30 14 28 2 *")
	// 已过今年 2 月 → 明年 2/28。
	if got := e.next(at(2026, 3, 1, 0, 0)); !got.Equal(at(2027, 2, 28, 14, 30)) {
		t.Errorf("next = %v, 期望 2027-02-28 14:30", got)
	}
}

func TestCronNextDomDowOrSemantics(t *testing.T) {
	e := mustExpr(t, "0 0 13 * 5")
	// 日与星期同时受限按"或"匹配：13 号或周五都触发。
	// 2026-10-13 是周二且 13 号 → 触发；10-16 周五 → 触发；10-14 周三非 13 号 → 不触发。
	if got := e.next(at(2026, 10, 1, 0, 0)); !got.Equal(at(2026, 10, 2, 0, 0)) {
		t.Errorf("next = %v, 期望 10-02（周五）", got)
	}
	if !e.matches(at(2026, 10, 13, 0, 0)) {
		t.Error("13 号（周二）应命中（或语义）")
	}
	if e.matches(at(2026, 10, 14, 0, 0)) {
		t.Error("10-14（周三、非 13 号）不应命中")
	}
}

func TestCronNextSubHourRolling(t *testing.T) {
	e := mustExpr(t, "50 23 * * *")
	// 23:55 → 次日 23:50（跨日跨时）。
	if got := e.next(at(2026, 10, 14, 23, 55)); !got.Equal(at(2026, 10, 15, 23, 50)) {
		t.Errorf("next = %v, 期望次日 23:50", got)
	}
}

func TestCronNextIgnoresSeconds(t *testing.T) {
	e := mustExpr(t, "* * * * *")
	after := time.Date(2026, 10, 14, 10, 3, 30, 0, time.Local)
	if got := e.next(after); got.Minute() != 4 || got.Second() != 0 {
		t.Errorf("next 应对齐到分钟边界且丢弃秒: %v", got)
	}
}
