//
//  Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//

package component

import (
	"encoding/json"

	"github.com/cloudwego/eino/schema"

	"ragflow/internal/tokenizer"
)

// agentToolMsgFloorTokens keeps a minimal readable slice of every tool result:
// large retrieval payloads share the remaining budget proportionally, but a
// fully emptied tool message would hide from the model that a call happened.
const agentToolMsgFloorTokens = 128

// agentOutputReserve returns the token count held back for the model's own
// completion inside the context window. Providers count input+output against
// the same limit, so the prompt budget must leave room for the answer.
func agentOutputReserve(maxTokens *int) int {
	if maxTokens != nil && *maxTokens > 0 {
		return *maxTokens
	}
	return 8192
}

// fitToolResultMessages bounds the total prompt size by truncating tool-result
// message contents in place-copy, preserving message order, roles and
// tool_call/tool-result pairing. Dropping messages would orphan tool_call ids
// and fail at the provider; truncation only shrinks the oversized retrieval
// payloads that accumulate across ReAct rounds.
//
// budget is the total prompt allowance in tokens (the caller subtracts the
// output reservation). Non-tool messages always keep their full content;
// tool messages split what remains proportionally to their own size, with a
// per-message floor so at least a marker of each result survives.
func fitToolResultMessages(msgs []*schema.Message, budget int) []*schema.Message {
	if budget <= 0 || len(msgs) == 0 {
		return msgs
	}
	fixed := 0
	toolSizes := make([]int, len(msgs))
	for i, m := range msgs {
		if m == nil {
			continue
		}
		tokens := tokenizer.NumTokensFromString(m.Content)
		if m.Role == schema.Tool {
			toolSizes[i] = tokens
			continue
		}
		if len(m.ToolCalls) > 0 {
			if args, err := json.Marshal(m.ToolCalls); err == nil {
				tokens += tokenizer.NumTokensFromString(string(args))
			}
		}
		fixed += tokens
	}
	toolBudget := budget - fixed
	total := fixed
	for _, s := range toolSizes {
		total += s
	}
	if total <= budget {
		return msgs
	}

	out := make([]*schema.Message, len(msgs))
	copy(out, msgs)
	toolCount := 0
	for _, s := range toolSizes {
		if s > 0 {
			toolCount++
		}
	}
	if toolBudget < toolCount*agentToolMsgFloorTokens {
		// The whole allowance cannot even give every tool message the floor:
		// fall back to the floor each so no tool result disappears entirely.
		for i, m := range out {
			if toolSizes[i] > 0 {
				c := *m
				c.Content = truncateToolContent(m.Content, agentToolMsgFloorTokens)
				out[i] = &c
			}
		}
		return out
	}
	// Every tool message first gets the floor; the rest of the budget splits
	// proportional to each message's original size.
	remaining := toolBudget - toolCount*agentToolMsgFloorTokens
	totalSize := 0
	for _, s := range toolSizes {
		totalSize += s
	}
	for i, m := range out {
		if toolSizes[i] == 0 {
			continue
		}
		limit := agentToolMsgFloorTokens
		if totalSize > 0 {
			limit += remaining * toolSizes[i] / totalSize
		}
		c := *m
		c.Content = truncateToolContent(m.Content, limit)
		out[i] = &c
	}
	return out
}

func truncateToolContent(content string, limit int) string {
	if tokenizer.NumTokensFromString(content) <= limit {
		return content
	}
	return tokenizer.TrimContentToTokenLimit(content, limit) + "\n[truncated]"
}
