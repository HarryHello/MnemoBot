// Package deliver 处理投递侧的文本拆分 (QQ 单条消息长度限制, §5.7).
package deliver

// SplitText 按上限拆分文本; 优先在换行处断开 (不早于半程), 无处可断时硬切.
func SplitText(text string, max int) []string {
	runes := []rune(text)
	if max <= 0 || len(runes) <= max {
		return []string{text}
	}
	var chunks []string
	for len(runes) > max {
		cut := max
		for i := max - 1; i >= max/2; i-- {
			if runes[i] == '\n' {
				cut = i + 1
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
