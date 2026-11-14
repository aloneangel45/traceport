package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

// SearchOptions configures cross-session search behavior.
type SearchOptions struct {
	Query string
	Regex bool
	Tags  []string
}

// SearchHit describes a single match within a session.
type SearchHit struct {
	Source   string `json:"source"`              // command, tag, frame, tool, checkpoint_file
	Step     int    `json:"step,omitempty"`      // 1-based frame index when applicable
	Type     string `json:"type,omitempty"`      // frame EventType when applicable
	FilePath string `json:"file_path,omitempty"` // matched checkpoint manifest path
	Snippet  string `json:"snippet"`
}

// SessionSearchResult groups all matches discovered inside a single recorded session.
type SessionSearchResult struct {
	SessionID string      `json:"session_id"`
	Command   string      `json:"command"`
	Status    string      `json:"status"`
	Tags      []string    `json:"tags,omitempty"`
	StartTime time.Time   `json:"start_time"`
	Matches   []SearchHit `json:"matches"`
}

// Search scans recorded sessions across command strings, tags, frame contents, tool invocations,
// and shadow-tree checkpoint manifest file paths.
func (e *Engine) Search(opts SearchOptions) ([]SessionSearchResult, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return nil, fmt.Errorf("search query must not be empty")
	}

	filterTags, err := storage.NormalizeTags(opts.Tags)
	if err != nil {
		return nil, err
	}

	var matcher func(string) bool
	if opts.Regex {
		re, err := regexp.Compile(opts.Query)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression %q: %w", opts.Query, err)
		}
		matcher = re.MatchString
	} else {
		needle := strings.ToLower(opts.Query)
		matcher = func(haystack string) bool {
			return strings.Contains(strings.ToLower(haystack), needle)
		}
	}

	sessions, err := e.store.ListSessions()
	if err != nil {
		return nil, err
	}

	workDir, _ := os.Getwd()
	if workDir == "" {
		workDir = e.store.RootDir()
	}
	treeStore, treeErr := checkpoint.NewTreeStore(workDir, e.store.RootDir())
	if treeErr == nil {
		defer treeStore.Close()
	}

	results := make([]SessionSearchResult, 0)

	for _, sess := range sessions {
		if len(filterTags) > 0 && !storage.MatchesAllTags(sess, filterTags) {
			continue
		}

		var hits []SearchHit

		if matcher(sess.Command) {
			hits = append(hits, SearchHit{
				Source:  "command",
				Snippet: compactSearchSnippet(sess.Command, 160),
			})
		}

		for _, tag := range sess.Tags {
			if matcher(tag) {
				hits = append(hits, SearchHit{
					Source:  "tag",
					Snippet: tag,
				})
			}
		}

		for _, f := range sess.Frames {
			if f.Content != "" && matcher(f.Content) {
				hits = append(hits, SearchHit{
					Source:  "frame",
					Step:    f.Index,
					Type:    string(f.Type),
					Snippet: compactSearchSnippet(f.Content, 160),
				})
			}

			if f.Tool != nil {
				var toolTextParts []string
				if f.Tool.Name != "" {
					toolTextParts = append(toolTextParts, f.Tool.Name)
				}
				if len(f.Tool.Arguments) > 0 {
					if rawArgs, mErr := json.Marshal(f.Tool.Arguments); mErr == nil {
						toolTextParts = append(toolTextParts, string(rawArgs))
					}
				}
				if f.Tool.Result != "" {
					toolTextParts = append(toolTextParts, f.Tool.Result)
				}
				combinedTool := strings.Join(toolTextParts, " ")
				if combinedTool != "" && matcher(combinedTool) {
					hits = append(hits, SearchHit{
						Source:  "tool",
						Step:    f.Index,
						Type:    string(f.Type),
						Snippet: compactSearchSnippet(combinedTool, 160),
					})
				}
			}
		}

		if treeStore != nil {
			cpHits := searchSessionCheckpoints(sess, treeStore, matcher)
			hits = append(hits, cpHits...)
		}

		if len(hits) > 0 {
			results = append(results, SessionSearchResult{
				SessionID: sess.ID,
				Command:   sess.Command,
				Status:    sess.Status,
				Tags:      sess.Tags,
				StartTime: sess.StartTime,
				Matches:   hits,
			})
		}
	}

	return results, nil
}

func searchSessionCheckpoints(sess model.Session, treeStore *checkpoint.TreeStore, matcher func(string) bool) []SearchHit {
	var mutationHits []SearchHit
	var prevFiles map[string]string

	for _, f := range sess.Frames {
		if f.GitSnapshot == "" || !treeStore.HasTree(f.GitSnapshot) {
			continue
		}
		manifest, err := treeStore.LoadManifest(f.GitSnapshot)
		if err != nil {
			continue
		}

		curFiles := make(map[string]string, len(manifest.Files))
		for _, entry := range manifest.Files {
			curFiles[entry.Path] = entry.SHA256
		}

		if prevFiles != nil {
			var addedPaths, modPaths, delPaths []string
			for p, sha := range curFiles {
				if !matcher(p) {
					continue
				}
				oldSHA, existed := prevFiles[p]
				if !existed {
					addedPaths = append(addedPaths, p)
				} else if oldSHA != sha {
					modPaths = append(modPaths, p)
				}
			}
			for p := range prevFiles {
				if !matcher(p) {
					continue
				}
				if _, stillThere := curFiles[p]; !stillThere {
					delPaths = append(delPaths, p)
				}
			}
			sort.Strings(addedPaths)
			sort.Strings(modPaths)
			sort.Strings(delPaths)

			for _, p := range addedPaths {
				mutationHits = append(mutationHits, SearchHit{
					Source:   "checkpoint_file",
					Step:     f.Index,
					Type:     string(model.EventCheckpoint),
					FilePath: p,
					Snippet:  "added " + p,
				})
			}
			for _, p := range modPaths {
				mutationHits = append(mutationHits, SearchHit{
					Source:   "checkpoint_file",
					Step:     f.Index,
					Type:     string(model.EventCheckpoint),
					FilePath: p,
					Snippet:  "modified " + p,
				})
			}
			for _, p := range delPaths {
				mutationHits = append(mutationHits, SearchHit{
					Source:   "checkpoint_file",
					Step:     f.Index,
					Type:     string(model.EventCheckpoint),
					FilePath: p,
					Snippet:  "deleted " + p,
				})
			}
		}

		prevFiles = curFiles
	}

	return mutationHits
}

func compactSearchSnippet(s string, maxRunes int) string {
	clean := strings.ReplaceAll(s, "\r", "")
	clean = strings.ReplaceAll(clean, "\n", " ")
	clean = strings.TrimSpace(clean)
	if maxRunes <= 0 || utf8.RuneCountInString(clean) <= maxRunes {
		return clean
	}
	runes := []rune(clean)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}
