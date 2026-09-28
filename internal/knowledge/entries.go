package knowledge

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

//go:embed docs/*.md
var docFiles embed.FS

const (
	ToolKnowledgeSearch = "knowledge_search"
	ToolKnowledgeRead   = "knowledge_read"

	// maxEntryBody bounds one entry, so a read never floods the model.
	maxEntryBody = 8 << 10
	searchLimit  = 5
	snippetRunes = 160
)

// RepoEntries splits every handbook file into one entry per level-two
// section; text before the first section is the file's own entry. Headings
// inside code fences do not split.
func RepoEntries() ([]store.KnowledgeEntry, error) {
	files, err := docFiles.ReadDir("docs")
	if err != nil {
		return nil, fmt.Errorf("knowledge: read docs: %w", err)
	}
	var entries []store.KnowledgeEntry
	for _, file := range files {
		raw, err := docFiles.ReadFile(path.Join("docs", file.Name()))
		if err != nil {
			return nil, fmt.Errorf("knowledge: read %s: %w", file.Name(), err)
		}
		title, sections, err := splitSections(string(raw))
		if err != nil {
			return nil, fmt.Errorf("knowledge: %s: %w", file.Name(), err)
		}
		for _, section := range sections {
			entry := store.KnowledgeEntry{Ref: file.Name(), Title: title, Body: tools.Sanitize(section.body)}
			if section.heading != "" {
				entry.Ref, entry.Title = file.Name()+"#"+section.heading, title+" / "+section.heading
			}
			if len(entry.Body) > maxEntryBody {
				return nil, fmt.Errorf("knowledge: %s is %d bytes, limit %d", entry.Ref, len(entry.Body), maxEntryBody)
			}
			entry.SHA256 = digest(entry.Title, entry.Body)
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

type section struct{ heading, body string }

func splitSections(doc string) (string, []section, error) {
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "# ") {
		return "", nil, fmt.Errorf("must start with a level-one title")
	}
	title := strings.TrimSpace(strings.TrimPrefix(lines[0], "# "))
	current := section{}
	var sections []section
	flush := func() {
		current.body = strings.TrimSpace(current.body)
		if current.body != "" {
			sections = append(sections, current)
		}
	}
	fenced := false
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "## ") {
			flush()
			current = section{heading: strings.TrimSpace(strings.TrimPrefix(line, "## "))}
		}
		current.body += line + "\n"
	}
	flush()
	return title, sections, nil
}

func digest(title, body string) string {
	sum := sha256.Sum256([]byte(title + "\n" + body))
	return hex.EncodeToString(sum[:])
}

// RepoSyncer is the store write SyncRepo needs.
type RepoSyncer interface {
	SyncRepoKnowledge(ctx context.Context, entries []store.KnowledgeEntry, now time.Time) (added, updated, deleted int, err error)
}

// SyncRepo makes the repository entries in the database equal to the
// handbook compiled into this binary.
func SyncRepo(ctx context.Context, db RepoSyncer) (string, error) {
	entries, err := RepoEntries()
	if err != nil {
		return "", err
	}
	added, updated, deleted, err := db.SyncRepoKnowledge(ctx, entries, time.Now())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d sections (+%d ~%d -%d)", len(entries), added, updated, deleted), nil
}

// Reader is the store read the knowledge tools need.
type Reader interface {
	SearchKnowledge(ctx context.Context, query, source string, limit int) ([]store.KnowledgeHit, error)
	GetKnowledge(ctx context.Context, id uint64) (store.KnowledgeEntry, error)
}

// RegisterTools adds knowledge_search and knowledge_read. Entries are
// reference material, never evidence: outputs say so and pass the Registry's
// sanitizing and truncation like every other tool.
func RegisterTools(registry *tools.Registry, reader Reader) error {
	if err := registry.Register(tools.ToolSpec{
		Name: ToolKnowledgeSearch,
		Description: "Full-text search of the knowledge base: repository handbook sections (component background, error meanings, past postmortems) " +
			"and reviewed incidents. Returns id, title, source and a snippet; read one with knowledge_read. Reference material, not evidence of this incident.",
		Timeout: 5 * time.Second, MaxOutput: 4096,
		Params: []tools.ParamSpec{
			{Name: "query", Description: "keywords or a short question, Chinese or English", Required: true},
			{Name: "source", Description: "optional: repo or incident"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct{ Query, Source string }
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid args: %w", err)
			}
			query := strings.TrimSpace(args.Query)
			if query == "" || len(query) > 256 {
				return "", fmt.Errorf("query must be 1-256 bytes")
			}
			if args.Source != "" && args.Source != store.KnowledgeRepo && args.Source != store.KnowledgeIncident {
				return "", fmt.Errorf("source must be repo or incident")
			}
			hits, err := reader.SearchKnowledge(ctx, query, args.Source, searchLimit)
			if err != nil {
				return "", err
			}
			type result struct {
				ID      uint64  `json:"id"`
				Title   string  `json:"title"`
				Source  string  `json:"source"`
				Ref     string  `json:"ref"`
				Score   float64 `json:"score"`
				Snippet string  `json:"snippet"`
			}
			results := make([]result, 0, len(hits))
			for _, hit := range hits {
				results = append(results, result{ID: hit.ID, Title: hit.Title, Source: hit.Source, Ref: hit.Ref, Score: hit.Score, Snippet: Snippet(hit.Body, query)})
			}
			out, err := json.Marshal(map[string]any{"results": results, "note": "reference material, not evidence of this incident"})
			return string(out), err
		},
	}); err != nil {
		return err
	}
	return registry.Register(tools.ToolSpec{
		Name:        ToolKnowledgeRead,
		Description: "Read one knowledge entry by the id knowledge_search returned. Reference material, not evidence of this incident.",
		Timeout:     5 * time.Second, MaxOutput: maxEntryBody,
		Params: []tools.ParamSpec{{Name: "id", Description: "entry id from knowledge_search", Required: true}},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct{ ID json.RawMessage }
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid args: %w", err)
			}
			id, err := tools.IntArg(args.ID)
			if err != nil || id <= 0 {
				return "", fmt.Errorf("id must be a positive integer")
			}
			entry, err := reader.GetKnowledge(ctx, uint64(id))
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("# %s\nsource: %s ref: %s updated_at: %s\n（参考资料，不是本次事件的证据）\n\n%s",
				entry.Title, entry.Source, entry.Ref, entry.UpdatedAt.UTC().Format(time.RFC3339), entry.Body), nil
		},
	})
}

// Snippet is the body around the first query term found, else its start.
func Snippet(body, query string) string {
	runes := []rune(body)
	start := 0
	for _, term := range append([]string{query}, strings.Fields(query)...) {
		if at := strings.Index(body, term); at >= 0 {
			start = max(len([]rune(body[:at]))-snippetRunes/4, 0)
			break
		}
	}
	end := min(start+snippetRunes, len(runes))
	return strings.Join(strings.Fields(string(runes[start:end])), " ")
}
