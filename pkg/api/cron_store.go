package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// cronDurableTTL 是 durable 任务的存活期：超过 7 天自动清理（对齐 comate
// scheduled_prompts 的过期语义）。在恢复时与每轮扫描时执行。
const cronDurableTTL = 7 * 24 * time.Hour

// cronDurableLimit 是 durable 任务总量上限：超限拒绝创建并明确报错。
const cronDurableLimit = 50

// cronStore 把 durable cron 任务落盘到 {ProjectRoot}/.agents/scheduled_prompts.json，
// 与 comate 的定时任务持久化同构：进程重启后恢复调度，7 天过期，总量上限 50。
// 非 durable 任务不经此存储，仅存内存。
type cronStore struct {
	path string
}

func newCronStore(projectRoot string) *cronStore {
	return &cronStore{path: filepath.Join(projectRoot, ".agents", "scheduled_prompts.json")}
}

// cronStoreRawFile 是读取侧的顶层结构：tasks 保留原始 JSON 以便逐条容错解析。
type cronStoreRawFile struct {
	Version int               `json:"version"`
	Tasks   []json.RawMessage `json:"tasks"`
}

// cronStoreFile 是写入侧的顶层结构（任务已校验，直接序列化）。
type cronStoreFile struct {
	Version int            `json:"version"`
	Tasks   []cronTaskJSON `json:"tasks"`
}

// cronTaskJSON 是单条 durable 任务的落盘形态（时间 RFC3339）。
type cronTaskJSON struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Expr      string    `json:"expr"`
	Prompt    string    `json:"prompt"`
	Recurring bool      `json:"recurring"`
	CreatedAt time.Time `json:"created_at"`
	NextRun   time.Time `json:"next_run"`
	LastRun   time.Time `json:"last_run"`
}

// load 读取并解析落盘文件。文件不存在或整体损坏时返回空（降级启动），
// 单条损坏跳过该条，绝不 crash。
func (s *cronStore) load() []CronTask {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			runtimeLogger.warnf("[api:cron] durable load failed: %v", err)
		}
		return nil
	}
	var file cronStoreRawFile
	if err := json.Unmarshal(data, &file); err != nil {
		runtimeLogger.warnf("[api:cron] durable 文件损坏，降级启动（忽略全部条目）: %v", err)
		return nil
	}
	var out []CronTask
	for i, raw := range file.Tasks {
		var entry cronTaskJSON
		if err := json.Unmarshal(raw, &entry); err != nil {
			runtimeLogger.warnf("[api:cron] durable 条目 %d 损坏，跳过: %v", i, err)
			continue
		}
		if entry.ID == "" || entry.SessionID == "" || entry.Expr == "" {
			runtimeLogger.warnf("[api:cron] durable 条目 %d 字段缺失，跳过", i)
			continue
		}
		out = append(out, CronTask{
			ID:        entry.ID,
			SessionID: entry.SessionID,
			Expr:      entry.Expr,
			Prompt:    entry.Prompt,
			Recurring: entry.Recurring,
			Durable:   true, // 落盘文件中的条目均为 durable 任务
			CreatedAt: entry.CreatedAt,
			NextRun:   entry.NextRun,
			LastRun:   entry.LastRun,
		})
	}
	return out
}

// save 原子落盘：先写同目录临时文件再 rename 覆盖，避免读侧看到写了一半的内容。
func (s *cronStore) save(tasks []CronTask) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("api: cron durable 目录创建失败: %w", err)
	}
	entries := make([]cronTaskJSON, 0, len(tasks))
	for _, t := range tasks {
		entries = append(entries, cronTaskJSON{
			ID:        t.ID,
			SessionID: t.SessionID,
			Expr:      t.Expr,
			Prompt:    t.Prompt,
			Recurring: t.Recurring,
			CreatedAt: t.CreatedAt,
			NextRun:   t.NextRun,
			LastRun:   t.LastRun,
		})
	}
	data, err := json.MarshalIndent(cronStoreFile{Version: 1, Tasks: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("api: cron durable 序列化失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("api: cron durable 写入失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("api: cron durable 替换失败: %w", err)
	}
	return nil
}
