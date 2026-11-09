package main

// Faithful lexical ranking port of CodexFree v1.9.1:
// src/mcp_catalog.rs::SearchIndex::{build,search}, SearchDocument::new,
// collect_schema_terms and tokenize. The in-process AgentGateway cache remains
// the single catalogue source. Only the tokenizer's CJK bigrams and the small
// natural-language query expansion below extend upstream behavior.

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

type mcpSearchDocument struct {
	entry       catalogEntry
	terms       map[string]float64
	length      float64
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
		tokens = append(tokens, strings.ToLower(string(current)))
		// Upstream uses Unicode alphanumeric words. Chinese natural-language
		// queries otherwise become one long word; overlap bigrams let matching
		// CJK metadata remain discoverable without a heavyweight tokenizer.
		var run []rune
		flushHan := func() {
			for i := 0; i < len(run); i++ {
				tokens = append(tokens, string(run[i]))
				if i+1 < len(run) {
					tokens = append(tokens, string(run[i:i+2]))
				}
			}
			run = nil
		}
		for _, ch := range current {
			if unicode.Is(unicode.Han, ch) {
				run = append(run, ch)
			} else if len(run) > 0 {
				flushHan()
			}
		}
		if len(run) > 0 {
			flushHan()
		}
		current = current[:0]
	}
	for i, ch := range rs {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
			var prev, next rune
			if i > 0 {
				prev = rs[i-1]
			}
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			if unicode.IsUpper(ch) && len(current) > 0 &&
				((unicode.IsLower(prev) || unicode.IsDigit(prev)) ||
					(unicode.IsUpper(prev) && unicode.IsLower(next))) {
				flush()
			}
			current = append(current, unicode.ToLower(ch))
		} else {
			flush()
		}
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
	frequency := map[string]float64{}
	addRankTerms(frequency, entry.SourceID, 1.5)
	addRankTerms(frequency, entry.Name, 5)
	addRankTerms(frequency, entry.Description, 2)
	title := ""
	if len(entry.RawTool) > 0 {
		var raw map[string]any
		if json.Unmarshal(entry.RawTool, &raw) == nil {
			if value, ok := raw["title"].(string); ok {
				title = value
				addRankTerms(frequency, title, 4)
			}
			appendRankSchema(frequency, raw["inputSchema"])
			appendRankSchema(frequency, raw["outputSchema"])
		}
	} else {
		// Compatibility with isolated cache fixtures that do not carry a raw
		// upstream schema. Production catalog entries always include RawTool.
		addRankTerms(frequency, entry.SearchText, 0.5)
	}
	size := 0.0
	for _, n := range frequency {
		size += n
	}
	if size < 1 {
		size = 1
	}
	return mcpSearchDocument{
		entry: entry, terms: frequency, length: size,
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
	{"记忆", "memory recall memos"},
	{"之前", "memory recall"},
	{"上次", "memory recall"},
	{"历史", "history memory"},
	{"搜索", "search"},
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
	avgLength := 0.0
	for _, entry := range entries {
		doc := newMCPRankDocument(entry)
		docs = append(docs, doc)
		avgLength += doc.length
		for term := range doc.terms {
			docFrequency[term]++
		}
	}
	if len(docs) == 0 {
		return nil
	}
	avgLength /= float64(len(docs))
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
		for term, occur := range queryFrequency {
			tf := doc.terms[term]
			if tf == 0 {
				continue
			}
			df := float64(docFrequency[term])
			idf := math.Log(1 + (count-df+0.5)/(df+0.5))
			norm := mcpBM25K1 * (1 - mcpBM25B + mcpBM25B*doc.length/avgLength)
			score += idf * (tf * (mcpBM25K1 + 1) / (tf + norm)) * (1 + math.Log(float64(occur)))
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
