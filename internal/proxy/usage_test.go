package proxy

import "testing"

func TestUsageIncludesCachedInput(t *testing.T) {
	var usage Usage
	usage.add(Usage{PromptTokens: 3, CompletionTokens: 2, CacheReadTokens: 20, CacheWriteTokens: 5})
	if usage.TotalTokens != 30 {
		t.Fatalf("total excludes cache: %+v", usage)
	}
	got := usageMap(usage)
	if got["prompt_tokens"] != 28 || got["total_tokens"] != 30 {
		t.Fatalf("OpenAI usage %+v", got)
	}
}
