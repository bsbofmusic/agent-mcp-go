package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexFreeTokenizeFixtures(t *testing.T) {
	fixtures := map[string]string{
		"renameFunction_by-address": "rename function by address",
		"readIDBMetadata":           "read idb metadata",
		"SearchMemory":              "search memory",
	}
	for source, want := range fixtures {
		got := strings.Join(mcpLexicalTokens(source), " ")
		if got != want {
			t.Fatalf("%q: got %q, want %q", source, got, want)
		}
	}
	chinese := strings.Join(mcpLexicalTokens("查看之前的记忆"), " ")
	if !strings.Contains(chinese, "记忆") {
		t.Fatalf("CJK bigrams omitted from %q", chinese)
	}
}

func TestCodexFreeBM25RankAndSchemas(t *testing.T) {
	fixtures := []catalogEntry{
		{SourceID: "memos", Name: "search_memory", Description: "Search historical memories", RawTool: json.RawMessage(`{"name":"search_memory","inputSchema":{"type":"object","properties":{"query":{"description":"semantic memory text","type":"string"}}}}`)},
		{SourceID: "docs", Name: "memory_card", Description: "Read a memory card", RawTool: json.RawMessage(`{"name":"memory_card","inputSchema":{"type":"object"}}`)},
		{SourceID: "browser", Name: "capture_page", Description: "Capture web screenshot", RawTool: json.RawMessage(`{"name":"capture_page","inputSchema":{"type":"object","properties":{"url":{"description":"website address"}}}}`)},
	}
	same := rankMCPEntries(fixtures, "search_memory")
	if len(same) == 0 || same[0].Name != "search_memory" {
		t.Fatalf("exact tool name not ranked first: %+v", same)
	}
	chinese := rankMCPEntries(fixtures, "帮我搜索之前的记忆")
	if len(chinese) == 0 || chinese[0].Name != "search_memory" {
		t.Fatalf("Chinese memory query not mapped to the matching canonical route: %+v", chinese)
	}
	image := rankMCPEntries(fixtures, "网页截图")
	if len(image) == 0 || image[0].Name != "capture_page" {
		t.Fatalf("Chinese browser query did not match screenshot metadata: %+v", image)
	}
	for _, result := range chinese {
		if result.Score <= 0 {
			t.Fatalf("BM25 returned nonpositive score: %+v", result)
		}
	}
	if len(rankMCPEntries(nil, "foo")) != 0 {
		t.Fatal("empty catalogue matched")
	}
}

func TestMCPRankChineseIntentAndNoise(t *testing.T) {
	entries := []catalogEntry{
		{SourceID: "fixture-memory", Name: "search_memory", Description: "Search saved memory preferences"},
		{SourceID: "fixture-patent", Name: "patent_lookup", Description: "Patent legal status and design publications"},
		{SourceID: "fixture-notes", Name: "read_note", Description: "Read knowledge base note content"},
		{SourceID: "fixture-history", Name: "search_sessions", Description: "Conversation session chat history records"},
		{SourceID: "fixture-community", Name: "reddit_posts", Description: "Reddit community discussion posts"},
		{SourceID: "fixture-store", Name: "shop_summary", Description: "Ecommerce store shop sales summary"},
		{SourceID: "fixture-web", Name: "web_search", Description: "General web internet search engines"},
		{SourceID: "fixture-dns", Name: "cloudflare_dns", Description: "Cloudflare domain DNS records"},
		{SourceID: "fixture-chart", Name: "planetary_chart", Description: "Planetary astrology chart calculation"},
		{SourceID: "fixture-product", Name: "amazon_product", Description: "Amazon product details and reviews"},
	}
	for _, query := range []string{"查找知识库里的笔记", "查找之前的会话历史记录", "查看电商店铺信息", "七政四余排盘"} {
		want := map[string]string{"查找知识库里的笔记": "fixture-notes", "查找之前的会话历史记录": "fixture-history", "查看电商店铺信息": "fixture-store", "七政四余排盘": "fixture-chart"}[query]
		got := sortedDiscoveryMatches(entries, query)
		if len(got) == 0 || got[0].SourceId != want {
			t.Errorf("%q: want %s first, got %+v", query, want, got)
		}
	}
	// Chinese prose in an unrelated schema must not create one-character hits.
	noise := []catalogEntry{{SourceID: "unrelated", Name: "write_entry", Description: "将我查到的信息写入指定的文档"}}
	for _, query := range []string{"的", "我", "查", "无关量子波函数"} {
		if got := sortedDiscoveryMatches(noise, query); len(got) != 0 {
			t.Errorf("unrelated query %q returned %+v", query, got)
		}
	}
	// A business noun must outweigh common search vocabulary and long prose.
	competitors := []catalogEntry{{SourceID: "business", Name: "inspect", Description: "Patent publications and legal information"}}
	for i := 0; i < 8; i++ {
		competitors = append(competitors, catalogEntry{SourceID: "generic", Name: "search_find_history", Description: "Search find historical web data and records"})
	}
	if got := sortedDiscoveryMatches(competitors, "搜索查找历史专利"); len(got) == 0 || got[0].SourceId != "business" {
		t.Errorf("generic query verbs drowned out patent intent: %+v", got)
	}
}

func TestMCPRankMixedScriptAndEnglish(t *testing.T) {
	entries := []catalogEntry{{SourceID: "domain", Name: "lookup_patent", Description: "Patent legal status"}, {SourceID: "noise", Name: "search_everything", Description: "Generic search"}}
	for _, query := range []string{"patent专利", "Patent专利法律状态", "patent legal status", "lookup_patent"} {
		got := sortedDiscoveryMatches(entries, query)
		if len(got) == 0 || got[0].SourceId != "domain" {
			t.Errorf("query %q: %+v", query, got)
		}
	}
	got := mcpLexicalTokens("patent专利")
	if !strings.Contains(" "+strings.Join(got, " ")+" ", " patent ") {
		t.Errorf("mixed-script word lost English token: %v", got)
	}
}
func TestCodexFreeRankDocsDoNotIndexSecretSchemaValues(t *testing.T) {
	entries := []catalogEntry{{Name: "search", SourceID: "safe", Description: "Public lookup",
		RawTool: json.RawMessage(`{"inputSchema":{"properties":{"token":{"type":"string","default":"SECRET_NOT_INDEXED"}}}}`)}}
	if got := rankMCPEntries(entries, "SECRET_NOT_INDEXED"); len(got) != 0 {
		t.Fatalf("unrecognized raw schema values should not be indexed: %+v", got)
	}
}
