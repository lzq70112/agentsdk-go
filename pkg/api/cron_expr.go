package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronMaxLookahead 是 next() 的搜索上限：4 年覆盖全部闰年与星期组合。
// 超过上限仍未命中（如 "0 0 30 2 *" 这类永不存在的日期组合）返回零值时间，
// 由调用方（AddCronTask）在创建时报错，而不是留下永不触发的任务。
const cronMaxLookahead = 4 * 366 * 24 * time.Hour

// cronExpr 是 5 字段 cron 表达式（分 时 日 月 星期）的解析结果。
// 每个字段展开为取值集合；domStar/dowStar 记录日/星期字段是否为字面 *，
// 供 dayMatches 实现 vixie-cron 的"两者同时受限按或匹配"语义。
type cronExpr struct {
	minutes map[int]bool // 0-59
	hours   map[int]bool // 0-23
	doms    map[int]bool // 1-31
	months  map[int]bool // 1-12
	dows    map[int]bool // 0-6（0=周日，输入 7 归一为 0）
	domStar bool
	dowStar bool
}

// parseCronExpr 解析标准 5 字段 cron 表达式。字段以空白分隔，每项支持
// *、单值、区间 a-b、步进 */n 与 a-b/n，逗号分隔多值。非法输入明确报错。
func parseCronExpr(spec string) (*cronExpr, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("api: cron 表达式需 5 个字段（分 时 日 月 星期），实际 %d 个", len(fields))
	}
	minutes, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("api: cron 分钟字段 %q: %w", fields[0], err)
	}
	hours, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("api: cron 小时字段 %q: %w", fields[1], err)
	}
	doms, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("api: cron 日字段 %q: %w", fields[2], err)
	}
	months, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("api: cron 月字段 %q: %w", fields[3], err)
	}
	dows, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("api: cron 星期字段 %q: %w", fields[4], err)
	}
	// 星期 7 与 0 同为周日，归一到 0，避免集合里出现越界键。
	if dows[7] {
		delete(dows, 7)
		dows[0] = true
	}
	return &cronExpr{
		minutes: minutes,
		hours:   hours,
		doms:    doms,
		months:  months,
		dows:    dows,
		domStar: fields[2] == "*",
		dowStar: fields[4] == "*",
	}, nil
}

// parseCronField 解析单个 cron 字段为取值集合，min/max 为该字段的合法值域。
func parseCronField(field string, min, max int) (map[int]bool, error) {
	out := make(map[int]bool)
	for _, term := range strings.Split(field, ",") {
		lo, hi, step, err := parseCronTerm(term, min, max)
		if err != nil {
			return nil, err
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	return out, nil
}

// parseCronTerm 解析单个逗号分隔项，返回展开区间 [lo,hi] 与步长。
// 支持形式：*、*/n、a、a/n（自 a 步进至值域上限）、a-b、a-b/n。
func parseCronTerm(term string, min, max int) (lo, hi, step int, err error) {
	step = 1
	base := term
	if idx := strings.Index(term, "/"); idx >= 0 {
		base = term[:idx]
		if step, err = strconv.Atoi(term[idx+1:]); err != nil {
			return 0, 0, 0, fmt.Errorf("步进 %q 不是数字", term[idx+1:])
		}
		if step <= 0 {
			return 0, 0, 0, fmt.Errorf("步进必须为正，实际 %d", step)
		}
	}
	if base == "*" {
		return min, max, step, nil
	}
	if strings.Count(base, "-") > 1 {
		return 0, 0, 0, fmt.Errorf("多重区间 %q", term)
	}
	parts := strings.Split(base, "-")
	lo, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("值 %q 不是数字", parts[0])
	}
	if len(parts) == 1 {
		if lo < min || lo > max {
			return 0, 0, 0, fmt.Errorf("值 %d 超出范围 [%d,%d]", lo, min, max)
		}
		if step > 1 {
			return lo, max, step, nil
		}
		return lo, lo, 1, nil
	}
	hi, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("区间终点 %q 不是数字", parts[1])
	}
	if lo < min || lo > max || hi < min || hi > max {
		return 0, 0, 0, fmt.Errorf("区间 [%d,%d] 超出范围 [%d,%d]", lo, hi, min, max)
	}
	if lo > hi {
		return 0, 0, 0, fmt.Errorf("反向区间 [%d,%d]", lo, hi)
	}
	return lo, hi, step, nil
}

// next 返回 after 之后（严格大于，对齐到分钟边界）的下一个匹配时刻；
// 4 年内无匹配返回零值时间。逐单位跳跃：月不匹配跳到下月 1 日，日不匹配
// 跳到次日 0 点，时不匹配跳到下一整点，分不匹配跳到下一分钟——保证不跳过
// 任何匹配点，同时把最坏迭代次数压到常数级。
func (e *cronExpr) next(after time.Time) time.Time {
	t := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, after.Location())
	t = t.Add(time.Minute)
	deadline := t.Add(cronMaxLookahead)
	for t.Before(deadline) {
		if e.matches(t) {
			return t
		}
		t = e.stepForward(t)
	}
	return time.Time{}
}

// matches 判断 t 是否为表达式的触发时刻（t 应已对齐到分钟边界）。
func (e *cronExpr) matches(t time.Time) bool {
	return e.minutes[t.Minute()] &&
		e.hours[t.Hour()] &&
		e.months[int(t.Month())] &&
		e.dayMatches(t)
}

// dayMatches 实现日/星期受限组合的 vixie-cron 语义：两者都不受限恒真；
// 只限其一按该域判定；两者同时受限按"或"匹配（13 号或周五都触发）。
func (e *cronExpr) dayMatches(t time.Time) bool {
	domOk := e.doms[t.Day()]
	dowOk := e.dows[int(t.Weekday())]
	switch {
	case e.domStar && e.dowStar:
		return true
	case e.domStar:
		return dowOk
	case e.dowStar:
		return domOk
	default:
		return domOk || dowOk
	}
}

// stepForward 从 t 跳到下一个可能匹配的候选时刻。跳跃保持粗粒度且不越界：
// 跳过的区间内必然不存在匹配点（对应单位已确定不匹配）。
func (e *cronExpr) stepForward(t time.Time) time.Time {
	if !e.months[int(t.Month())] {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
	}
	if !e.dayMatches(t) {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
	}
	if !e.hours[t.Hour()] {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour)
	}
	return t.Add(time.Minute)
}
