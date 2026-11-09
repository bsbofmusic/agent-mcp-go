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

func TestCodexFreeRankDocsDoNotIndexSecretSchemaValues(t *testing.T) {
	entries := []catalogEntry{{Name: "search", SourceID: "safe", Description: "Public lookup",
		RawTool: json.RawMessage(`{"inputSchema":{"properties":{"token":{"type":"string","default":"SECRET_NOT_INDEXED"}}}}`)}}
	if got := rankMCPEntries(entries, "SECRET_NOT_INDEXED"); len(got) != 0 {
		t.Fatalf("unrecognized raw schema values should not be indexed: %+v", got)
	}
}
