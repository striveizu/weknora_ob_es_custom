package repository

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/Tencent/WeKnora/internal/types"
)

// JSON_SEARCH matches decoded array elements, not serialized JSON. Binding an
// explicit escape character preserves IDs containing LIKE wildcard characters.
func mysqlWikiSourcePredicate(id string) (string, []interface{}) {
	needle, _ := json.Marshal(id)
	return "(JSON_CONTAINS(source_refs, ?) OR JSON_SEARCH(source_refs, 'one', ?, ?) IS NOT NULL)",
		[]interface{}{string(needle), escapeLikePattern(id+"|") + "%", likeEscapeChar}
}

// The PoC does not depend on an eventually consistent ES index for entity
// identity. Page lightweight projections are paged through SQL; the top-N set
// remains bounded. Large corpora should add the dedicated Wiki ES candidate
// index after recall and outbox consistency are measured.
func (r *wikiPageRepository) findSimilarPagesMySQL(ctx context.Context, kbID, query string, pageTypes []string, limit int) ([]*types.WikiPageLite, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	if len(pageTypes) == 0 {
		pageTypes = []string{types.WikiPageTypeEntity, types.WikiPageTypeConcept}
	}
	type ranked struct {
		page  *types.WikiPageLite
		score float64
	}
	best := make([]ranked, 0, limit+1)
	cursor := ""
	for {
		var pages []struct {
			types.WikiPageLite
			ID string
		}
		q := r.db.WithContext(ctx).Model(&types.WikiPage{}).
			Select("id, slug, title, page_type, status, aliases, out_links").
			Where("knowledge_base_id = ? AND page_type IN ? AND status <> ?", kbID, pageTypes, types.WikiPageStatusArchived).
			Where("id > ?", cursor).Order("id ASC").Limit(250)
		if err := q.Scan(&pages).Error; err != nil {
			return nil, err
		}
		for i := range pages {
			p := &pages[i].WikiPageLite
			score := wikiBigramSimilarity(query, p.Title)
			for _, alias := range p.Aliases {
				if v := wikiBigramSimilarity(query, alias); v > score {
					score = v
				}
			}
			if score < 0.1 {
				continue
			}
			best = append(best, ranked{p, score})
			sort.Slice(best, func(i, j int) bool {
				if best[i].score == best[j].score {
					return best[i].page.Slug < best[j].page.Slug
				}
				return best[i].score > best[j].score
			})
			if len(best) > limit {
				best = best[:limit]
			}
		}
		if len(pages) < 250 {
			break
		}
		cursor = pages[len(pages)-1].ID
	}
	out := make([]*types.WikiPageLite, 0, len(best))
	for _, row := range best {
		out = append(out, row.page)
	}
	return out, nil
}

func wikiBigramSimilarity(a, b string) float64 {
	fold := func(s string) []rune {
		return []rune(strings.Map(func(c rune) rune {
			if unicode.IsSpace(c) {
				return -1
			}
			return unicode.ToLower(c)
		}, s))
	}
	x, y := fold(a), fold(b)
	if string(x) == string(y) && len(x) > 0 {
		return 1
	}
	grams := func(s []rune) map[string]bool {
		m := map[string]bool{}
		if len(s) == 1 {
			m[string(s)] = true
		}
		for i := 0; i+1 < len(s); i++ {
			m[string(s[i:i+2])] = true
		}
		return m
	}
	g, h := grams(x), grams(y)
	intersection := 0
	for key := range g {
		if h[key] {
			intersection++
		}
	}
	union := len(g) + len(h) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
