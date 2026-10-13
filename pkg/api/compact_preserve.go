package api

import (
	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// 压缩优先级约定：带 synthetic 标记的消息是平台注入的环境事实（子 agent
// 结果、系统提醒等），在任意压缩路径（主动压缩、反应式压缩、微压缩）中
// 原样保留、永不压缩。防的事故：长会话压缩后 agent 不知道自己收到过什么
// 注入事实（"压缩后失忆"）。新的注入路径（见注入路由）写入 history 时
// 必须带上该标记，即自动获得豁免。
const (
	metadataSynthetic    = "api.synthetic"
	metadataSyntheticKey = "api.synthetic_type"
)

// isSyntheticMessage 判定消息是否为平台注入的合成消息（环境事实类）。
func isSyntheticMessage(msg message.Message) bool {
	if msg.Metadata == nil {
		return false
	}
	flag, ok := msg.Metadata[metadataSynthetic].(bool)
	return ok && flag
}

// partitionSynthetic 把待压缩区间的消息分成"原样保留的合成消息"与"送入
// 摘要的普通消息"两段，各自保持原有相对顺序。
func partitionSynthetic(msgs []message.Message) (preserve, rest []message.Message) {
	for i := range msgs {
		if isSyntheticMessage(msgs[i]) {
			preserve = append(preserve, msgs[i])
		} else {
			rest = append(rest, msgs[i])
		}
	}
	return preserve, rest
}
