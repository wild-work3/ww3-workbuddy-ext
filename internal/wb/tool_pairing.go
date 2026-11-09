package wb

// 工具调用配对（旧版 upstream/tool_pairing.go 语义，搬运自参考实现 resolveToolPairing）。
//
// 背景：OpenAI 兼容协议要求带 tool_calls 的 assistant 消息，其每个 tool_call id 都必须有
// 对应的一条 role:tool 结果；反之 role:tool 也必须有前置 tool_call。缺任一侧上游都 400
// 拒绝整个请求，且坏历史会被每次都重放 → 整条会话报废。网关是最后一道防线。

// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool 消息
// 挪到整组之后，保证同批结果在 wire 上连续（否则上游判 11148 tool_call_sequence_broken）。
//
//	assistant [c00 c01] | tool c00 | X | tool c01  →  assistant [c00 c01] | tool c00 | tool c01 | X
//
// 无插入消息时零改动（返回原 slice）。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		out = append(out, messages[i])
		i++
		var results, between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant 后没有结果：交 cleanupOrphanToolCalls 处理
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：
			// 一旦收进 between，它自己的那批结果永远得不到重排。必须 break 交还外层。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// cleanupOrphanToolCalls 双侧按同一份 keepCalls 对称裁剪孤儿调用与孤儿结果。
// 无工具流量时零改动（返回原 slice）。
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：只留有结果的调用，过滤后为空则删键。
	// 绝不整批删除：那会造出「无 tool_calls 的 assistant + 孤儿 tool」，上游判 11148。
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); keepCalls[id] {
				keptCalls = append(keptCalls, tc)
			}
		}
		if len(keptCalls) == len(tcs) {
			continue
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	// 2) role:tool：对应 tool_call 未被保留则整条删除。
	kept := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}
