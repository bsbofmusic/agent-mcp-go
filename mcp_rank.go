package main

// BM25 scoring follows CodexFree v1.9.1's SearchIndex/SearchDocument skeleton.
// Local extensions split Han/Latin runs, index Han bigrams, expand bilingual
// query vocabulary and reduce generic query-verb weights. The live AgentGateway
// catalogue remains the sole source of route identities and tool metadata.

import (
	"encoding/json"
	"math"
	"strings"
	"unicode"
)

const (
	mcpBM25K1 = 1.2
	mcpBM25B  = 0.75
)

type mcpRankField struct {
	terms  map[string]float64
	length float64
	weight float64
}

type mcpSearchDocument struct {
	entry       catalogEntry
	terms       map[string]float64
	fields      []mcpRankField
	normalized  string
	normalTitle string
}

func mcpLexicalTokens(input string) []string {
	rs := []rune(input)
	var tokens []string
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		han := unicode.Is(unicode.Han, current[0])
		if !han || len(current) > 1 {
			tokens = append(tokens, strings.ToLower(string(current)))
		}
		if han && len(current) > 2 {
			for i := 0; i+1 < len(current); i++ {
				tokens = append(tokens, string(current[i:i+2]))
			}
		}
		current = current[:0]
	}
	for i, ch := range rs {
		if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) {
			flush()
			continue
		}
		if len(current) > 0 {
			prev := rs[i-1]
			var next rune
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			if unicode.Is(unicode.Han, ch) != unicode.Is(unicode.Han, current[0]) ||
				(unicode.IsUpper(ch) && ((unicode.IsLower(prev) || unicode.IsDigit(prev)) || (unicode.IsUpper(prev) && unicode.IsLower(next)))) {
				flush()
			}
		}
		current = append(current, unicode.ToLower(ch))
	}
	flush()
	return tokens
}

func addRankTerms(terms map[string]float64, phrase string, weight float64) {
	for _, word := range mcpLexicalTokens(phrase) {
		terms[word] += weight
	}
}

func appendRankSchema(terms map[string]float64, value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			switch key {
			case "properties", "$defs", "definitions":
				if nested, ok := item.(map[string]any); ok {
					for name, child := range nested {
						addRankTerms(terms, name, 2)
						appendRankSchema(terms, child)
					}
				}
			case "title":
				if text, ok := item.(string); ok {
					addRankTerms(terms, text, 1.5)
				}
			case "description":
				if text, ok := item.(string); ok {
					addRankTerms(terms, text, 1)
				}
			case "required":
				if values, ok := item.([]any); ok {
					for _, item := range values {
						if text, ok := item.(string); ok {
							addRankTerms(terms, text, 1.25)
						}
					}
				}
			case "enum":
				if values, ok := item.([]any); ok {
					for _, item := range values {
						if text, ok := item.(string); ok {
							addRankTerms(terms, text, 0.4)
						}
					}
				}
			default:
				appendRankSchema(terms, item)
			}
		}
	case []any:
		for _, value := range v {
			appendRankSchema(terms, value)
		}
	}
}

func newMCPRankDocument(entry catalogEntry) mcpSearchDocument {
	// Keep execution identity separate from incidental schema vocabulary.
	fields := make([]mcpRankField, 6)
	for i, weight := range []float64{1.5, 5, 4, 2, 1, 0.25} {
		fields[i] = mcpRankField{terms: map[string]float64{}, weight: weight}
	}
	addRankTerms(fields[0].terms, entry.SourceID, 1)
	addRankTerms(fields[1].terms, entry.Name, 1)
	addRankTerms(fields[3].terms, entry.Description, 1)
	title := ""
	if len(entry.RawTool) > 0 {
		var raw map[string]any
		if json.Unmarshal(entry.RawTool, &raw) == nil {
			if value, ok := raw["title"].(string); ok {
				title = value
				addRankTerms(fields[2].terms, title, 1)
			}
			appendRankSchema(fields[4].terms, raw["inputSchema"])
			appendRankSchema(fields[5].terms, raw["outputSchema"])
		}
	} else {
		// Compatibility for isolated fixtures without an upstream schema.
		addRankTerms(fields[4].terms, entry.SearchText, 0.5)
	}
	frequency := map[string]float64{}
	for i := range fields {
		for term, n := range fields[i].terms {
			// Repeating a capability claim cannot create additional evidence.
			fields[i].terms[term] = math.Min(n, 1)
			fields[i].length += fields[i].terms[term]
			frequency[term] = 1
		}
	}
	return mcpSearchDocument{
		entry: entry, terms: frequency, fields: fields,
		normalized:  strings.Join(mcpLexicalTokens(entry.Name), " "),
		normalTitle: strings.Join(mcpLexicalTokens(title), " "),
	}
}

// These are only query synonyms, never fixed MCP route owners or a second
// registry. Route identities and exact tool schemas still come from
// AgentGateway and are verified on each real tool call.
var chineseMCPQueryWords = []struct {
	term, english string
}{
	{"记忆", "memory recall"},
	{"回忆", "memory recall"},
	{"偏好", "preference"},
	{"历史", "history"},
	{"会话", "session sessions conversation transcript"},
	{"聊天", "chat conversation session sessions transcript"},
	{"笔记", "note notes"},
	{"知识库", "knowledge base"},
	{"社区", "community"},
	{"讨论", "discussion"},
	{"帖子", "post"},
	{"电商", "ecommerce"},
	{"店铺", "store shop"},
	{"商店", "store shop"},
	{"销售", "sales"},
	{"排盘", "astrology chart"},
	{"星盘", "astrology chart"},
	{"互联网", "internet web"},
	{"联网", "internet web"},
	{"域名", "domain"},
	{"搜索", "search"},
	{"检索", "search"},
	{"查找", "search find"},
	{"浏览器", "browser chrome"},
	{"网页", "web browser"},
	{"截图", "screenshot image"},
	{"文件", "file"},
	{"专利", "patent"},
	{"亚马逊", "amazon"},
	{"商品", "product"},
	{"表格", "sheet spreadsheet"},
	{"天气", "weather"},
}

func searchQueryTokens(query string) []string {
	tokens := mcpLexicalTokens(query)
	for _, entry := range chineseMCPQueryWords {
		if strings.Contains(query, entry.term) {
			tokens = append(tokens, mcpLexicalTokens(entry.english)...)
		}
	}
	return tokens
}

func rankMCPEntries(entries []catalogEntry, query string) []SearchMatch {
	docs := make([]mcpSearchDocument, 0, len(entries))
	docFrequency := map[string]int{}
	avgLengths := make([]float64, 6)
	for _, entry := range entries {
		doc := newMCPRankDocument(entry)
		docs = append(docs, doc)
		for i, field := range doc.fields {
			avgLengths[i] += field.length
		}
		for term := range doc.terms {
			docFrequency[term]++
		}
	}
	if len(docs) == 0 {
		return nil
	}
	for i := range avgLengths {
		avgLengths[i] = math.Max(1, avgLengths[i]/float64(len(docs)))
	}
	queryTerms := searchQueryTokens(query)
	queryFrequency := map[string]int{}
	for _, term := range queryTerms {
		queryFrequency[term]++
	}
	normalQuery := strings.Join(mcpLexicalTokens(query), " ")
	var result []SearchMatch
	count := float64(len(docs))
	for _, doc := range docs {
		score := 0.0
		fieldScores := make([]float64, len(doc.fields))
		for term, occur := range queryFrequency {
			if doc.terms[term] == 0 {
				continue
			}
			df := float64(docFrequency[term])
			idf := math.Log(1 + (count-df+0.5)/(df+0.5))
			weight := 1.0
			switch term {
			case "search", "find", "lookup", "get", "list", "read", "搜索", "查找", "检索", "查看":
				weight = 0.15
			}
			for i, field := range doc.fields {
				tf := field.terms[term]
				if tf == 0 {
					continue
				}
				norm := mcpBM25K1 * (1 - mcpBM25B + mcpBM25B*field.length/avgLengths[i])
				fieldScores[i] += field.weight * weight * idf * (tf * (mcpBM25K1 + 1) / (tf + norm)) * (1 + math.Log(float64(occur)))
			}
		}
		for i, contribution := range fieldScores {
			// Supporting prose/schema evidence has a bounded, monotonic budget.
			// Additional query matches cannot reduce relevance or outgrow identity.
			if i >= 3 {
				contribution = doc.fields[i].weight * contribution / (doc.fields[i].weight + contribution)
			}
			score += contribution
		}
		if normalQuery != "" && doc.normalized == normalQuery {
			score += 12
		} else if normalQuery != "" && strings.Contains(doc.normalized, normalQuery) {
			score += 4
		}
		if normalQuery != "" && doc.normalTitle == normalQuery {
			score += 6
		}
		if score <= 0 {
			continue
		}
		desc := []rune(doc.entry.Description)
		if len(desc) > 600 {
			desc = append(desc[:600], '…')
		}
		result = append(result, SearchMatch{
			SourceId: doc.entry.SourceID, ToolId: doc.entry.Name,
			Name: doc.entry.Name, Description: string(desc),
			Score: math.Round(score*1e4) / 1e4,
		})
	}
	return result
}
