package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func rankFieldFixture(source, name, title, description string, input, output any) catalogEntry {
	raw, err := json.Marshal(map[string]any{"name": name, "title": title, "description": description, "inputSchema": input, "outputSchema": output})
	if err != nil {
		panic(err)
	}
	return catalogEntry{SourceID: source, Name: name, Description: description, RawTool: raw}
}

func TestMCPRankFieldsExecutorAndCapabilityReport(t *testing.T) {
	object := map[string]any{"type": "object"}
	topics := "patent legal status memory conversation session history 专利法律状态记忆会话聊天历史 "
	entries := []catalogEntry{
		rankFieldFixture("route-a", "lookup_patent", "Patent lookup", "Search patent publications and legal status", object, object),
		rankFieldFixture("route-b", "search_sessions", "Conversation history", "Find conversation session history records", object, object),
		rankFieldFixture("route-c", "search_memory", "Memory recall", "Search saved memory preferences", object, object),
		rankFieldFixture("route-d", "inspect_services", "Capability report", "Report supported capabilities and missing capabilities. 报告当前支持和缺失的能力。", object,
			map[string]any{"type": "object", "properties": map[string]any{"supported": map[string]any{"description": strings.Repeat(topics, 24)}}}),
		rankFieldFixture("route-e", "summarize_inventory", "Service inventory", "Summarize supported services. "+strings.Repeat(topics, 24), object, object),
	}
	cases := []struct{ query, want string }{
		{"搜索专利法律状态", "lookup_patent"}, {"patent legal status", "lookup_patent"},
		{"查找之前的会话历史记录", "search_sessions"}, {"conversation session history", "search_sessions"},
		{"搜索记忆", "search_memory"}, {"search memory", "search_memory"},
		{"报告当前支持和缺失的能力", "inspect_services"}, {"report missing capabilities", "inspect_services"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := sortedDiscoveryMatches(entries, tc.query)
			if len(got) == 0 || got[0].Name != tc.want {
				t.Fatalf("want %s first, got %+v", tc.want, got)
			}
		})
	}
	if got := sortedDiscoveryMatches(entries, "zzzxqvunknowncapability"); len(got) != 0 {
		t.Fatalf("unknown query matched: %+v", got)
	}
}

func TestMCPRankFieldsSchemaOnlyRecallAndIdentity(t *testing.T) {
	object := map[string]any{"type": "object"}
	entries := []catalogEntry{
		rankFieldFixture("unlabeled-a", "run_query", "Database query", "Execute a database request", map[string]any{"type": "object", "properties": map[string]any{"publication_number": map[string]any{"type": "string", "description": "Patent publication identifier"}}}, object),
		rankFieldFixture("unlabeled-b", "inspect_services", "Capability report", "Report supported service capabilities", object, object),
	}
	for _, query := range []string{"publication_number", "Patent publication identifier", "run_query", "Database query"} {
		got := sortedDiscoveryMatches(entries, query)
		if len(got) == 0 || got[0].Name != "run_query" {
			t.Errorf("schema/name/title recall lost for %q: %+v", query, got)
		}
	}
}

func TestMCPRankFieldsRepeatedDescriptionIsBounded(t *testing.T) {
	object := map[string]any{"type": "object"}
	short := rankFieldFixture("same-route", "inspect_services", "Capability report", "Report supported services. patent legal status", object, object)
	long := rankFieldFixture("same-route", "inspect_services", "Capability report", "Report supported services. "+strings.Repeat("patent legal status ", 200), object, object)
	score := func(entry catalogEntry) float64 {
		got := rankMCPEntries([]catalogEntry{entry}, "patent legal status")
		if len(got) == 0 {
			t.Fatal("description recall lost")
		}
		return got[0].Score
	}
	if a, b := score(short), score(long); b > a {
		t.Fatalf("repetition increased relevance: short=%g repeated=%g", a, b)
	}
}
