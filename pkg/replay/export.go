package replay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/redact"
)

// ExportOptions configures portable session export formatting and secret scrubbing.
type ExportOptions struct {
	Format string // "json" (default), "markdown" (or "md")
	Redact bool
}

// ExportSession renders sessionID as either a self-contained monolithic JSON document
// (compatible with agentfixture import -traceport) or a Markdown flight audit report.
func (e *Engine) ExportSession(sessionID string, opts ExportOptions) ([]byte, error) {
	session, err := e.store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if session.Frames == nil {
		session.Frames = []model.Frame{}
	}
	session.FrameCount = len(session.Frames)

	if opts.Redact {
		session = redact.Session(session)
	}

	format := strings.ToLower(strings.TrimSpace(opts.Format))
	if format == "" {
		format = "json"
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(session, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal session json: %w", err)
		}
		raw = append(raw, '\n')
		return raw, nil
	case "markdown", "md":
		md := e.renderMarkdownAuditReport(session, opts.Redact)
		return []byte(md), nil
	default:
		return nil, fmt.Errorf("unsupported export format %q (expected: json, markdown, md)", opts.Format)
	}
}

type checkpointFrameRef struct {
	step  int
	label string
	hash  string
}

func (e *Engine) renderMarkdownAuditReport(session *model.Session, redacted bool) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# TracePort Flight Audit Report: `%s`\n\n", session.ID))
	if redacted {
		sb.WriteString("> **Security Notice:** Credential redaction (`--redact`) was applied prior to export.\n\n")
	}

	startedStr := session.StartTime.UTC().Format(time.RFC3339)
	endedStr := "active"
	durationStr := "active"
	if !session.EndTime.IsZero() {
		endedStr = session.EndTime.UTC().Format(time.RFC3339)
		durationStr = session.EndTime.Sub(session.StartTime).Round(time.Millisecond).String()
	}

	tagsStr := "none"
	if len(session.Tags) > 0 {
		tagsStr = strings.Join(session.Tags, ", ")
	}

	sb.WriteString("## Session Metadata\n\n")
	sb.WriteString("| Field | Value |\n")
	sb.WriteString("| :--- | :--- |\n")
	sb.WriteString(fmt.Sprintf("| Session ID | `%s` |\n", escapeMarkdownTableCell(session.ID)))
	sb.WriteString(fmt.Sprintf("| Command | `%s` |\n", escapeMarkdownTableCell(session.Command)))
	sb.WriteString(fmt.Sprintf("| Status | `%s` |\n", escapeMarkdownTableCell(session.Status)))
	sb.WriteString(fmt.Sprintf("| Exit Code | `%d` |\n", session.ExitCode))
	sb.WriteString(fmt.Sprintf("| Started | `%s` |\n", startedStr))
	sb.WriteString(fmt.Sprintf("| Ended | `%s` |\n", endedStr))
	sb.WriteString(fmt.Sprintf("| Duration | `%s` |\n", durationStr))
	sb.WriteString(fmt.Sprintf("| Frames | `%d` |\n", len(session.Frames)))
	sb.WriteString(fmt.Sprintf("| Tags | `%s` |\n\n", escapeMarkdownTableCell(tagsStr)))

	// Checkpoint summary & file delta overview
	sb.WriteString("## Checkpoint & File Delta Overview\n\n")
	var checkpoints []checkpointFrameRef
	for _, f := range session.Frames {
		if f.GitSnapshot != "" {
			checkpoints = append(checkpoints, checkpointFrameRef{
				step:  f.Index,
				label: f.Content,
				hash:  f.GitSnapshot,
			})
		}
	}

	if len(checkpoints) == 0 {
		sb.WriteString("No shadow-tree checkpoints recorded in this session.\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Recorded **%d** workspace checkpoint(s):\n\n", len(checkpoints)))
		sb.WriteString("| Step | Label | Tree Digest |\n")
		sb.WriteString("| :--- | :--- | :--- |\n")
		for _, cp := range checkpoints {
			shortHash := cp.hash
			if len(shortHash) > 12 {
				shortHash = shortHash[:12]
			}
			label := cp.label
			if strings.TrimSpace(label) == "" {
				label = "Workspace snapshot"
			}
			sb.WriteString(fmt.Sprintf("| `#%d` | %s | `%s` |\n", cp.step, escapeMarkdownTableCell(label), shortHash))
		}
		sb.WriteString("\n")

		e.appendManifestDeltaMarkdown(&sb, session, checkpoints)
	}

	// Chronological Frame Log
	sb.WriteString("## Chronological Frame Log\n\n")
	if len(session.Frames) == 0 {
		sb.WriteString("_No frames recorded._\n")
		return sb.String()
	}

	for _, f := range session.Frames {
		ts := f.Timestamp.UTC().Format("15:04:05.000Z")
		headerExtra := ""
		if f.GitSnapshot != "" {
			short := f.GitSnapshot
			if len(short) > 12 {
				short = short[:12]
			}
			headerExtra = fmt.Sprintf(" (`tree:%s`)", short)
		}
		sb.WriteString(fmt.Sprintf("### Step #%d — `%s` (%s)%s\n\n", f.Index, f.Type, ts, headerExtra))

		excerpt := truncateUTF8RunesWithSuffix(f.Content, 2000, "\n... [truncated]")
		if strings.TrimSpace(excerpt) != "" {
			fence := tildeFenceForContent(excerpt)
			sb.WriteString(fence + "text\n")
			sb.WriteString(excerpt)
			if !strings.HasSuffix(excerpt, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString(fence + "\n\n")
		}

		if f.Tool != nil {
			sb.WriteString(fmt.Sprintf("- **Tool:** `%s`", escapeMarkdownTableCell(f.Tool.Name)))
			if f.Tool.DurationMs > 0 {
				sb.WriteString(fmt.Sprintf(" (`%dms`)", f.Tool.DurationMs))
			}
			sb.WriteString("\n")
			if len(f.Tool.Arguments) > 0 {
				if argBytes, err := json.Marshal(f.Tool.Arguments); err == nil {
					sb.WriteString(fmt.Sprintf("- **Arguments:** `%s`\n", escapeMarkdownTableCell(string(argBytes))))
				}
			}
			if strings.TrimSpace(f.Tool.Result) != "" {
				resExcerpt := truncateUTF8RunesWithSuffix(f.Tool.Result, 1000, "... [truncated]")
				sb.WriteString(fmt.Sprintf("- **Result:** `%s`\n", escapeMarkdownTableCell(resExcerpt)))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func (e *Engine) appendManifestDeltaMarkdown(sb *strings.Builder, _ *model.Session, checkpoints []checkpointFrameRef) {
	treeStore, err := checkpoint.NewTreeStore(e.store.RootDir(), e.store.RootDir())
	if err != nil {
		return
	}
	defer treeStore.Close()

	first := checkpoints[0]
	last := checkpoints[len(checkpoints)-1]

	firstManifest, errFirst := treeStore.LoadManifest(first.hash)
	if errFirst != nil {
		sb.WriteString("_Shadow-tree manifests unavailable in local store for delta computation._\n\n")
		return
	}

	if first.hash == last.hash {
		sb.WriteString(fmt.Sprintf(
			"**File Delta (Step `#%d` -> Step `#%d`):** No net filesystem mutations (`%d` tracked files unchanged).\n\n",
			first.step,
			last.step,
			len(firstManifest.Files),
		))
		return
	}

	lastManifest, errLast := treeStore.LoadManifest(last.hash)
	if errLast != nil {
		sb.WriteString("_Final shadow-tree manifest unavailable in local store for delta computation._\n\n")
		return
	}

	firstMap := make(map[string]checkpoint.FileEntry, len(firstManifest.Files))
	for _, f := range firstManifest.Files {
		firstMap[f.Path] = f
	}
	lastMap := make(map[string]checkpoint.FileEntry, len(lastManifest.Files))
	for _, f := range lastManifest.Files {
		lastMap[f.Path] = f
	}

	allPathsSet := make(map[string]struct{}, len(firstMap)+len(lastMap))
	for p := range firstMap {
		allPathsSet[p] = struct{}{}
	}
	for p := range lastMap {
		allPathsSet[p] = struct{}{}
	}
	allPaths := make([]string, 0, len(allPathsSet))
	for p := range allPathsSet {
		allPaths = append(allPaths, p)
	}
	sort.Strings(allPaths)

	var added, modified, deleted []string
	unchanged := 0

	for _, p := range allPaths {
		a, inA := firstMap[p]
		b, inB := lastMap[p]
		switch {
		case !inA && inB:
			added = append(added, fmt.Sprintf("+ `%s` (%d B)", escapeMarkdownTableCell(p), b.Size))
		case inA && !inB:
			deleted = append(deleted, fmt.Sprintf("- `%s` (was %d B)", escapeMarkdownTableCell(p), a.Size))
		case a.SHA256 != b.SHA256:
			modified = append(modified, fmt.Sprintf("~ `%s` (%d B -> %d B)", escapeMarkdownTableCell(p), a.Size, b.Size))
		default:
			unchanged++
		}
	}

	sb.WriteString(fmt.Sprintf(
		"### File Delta (Step `#%d` -> Step `#%d`)\n\n- **Added:** %d\n- **Modified:** %d\n- **Deleted:** %d\n- **Unchanged:** %d\n\n",
		first.step,
		last.step,
		len(added),
		len(modified),
		len(deleted),
		unchanged,
	))

	if len(added)+len(modified)+len(deleted) > 0 {
		sb.WriteString("#### Changed Files\n\n")
		for _, item := range added {
			sb.WriteString("- " + item + "\n")
		}
		for _, item := range modified {
			sb.WriteString("- " + item + "\n")
		}
		for _, item := range deleted {
			sb.WriteString("- " + item + "\n")
		}
		sb.WriteString("\n")
	}
}

func escapeMarkdownTableCell(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "`", "'")
	return s
}

func truncateUTF8RunesWithSuffix(s string, maxRunes int, suffix string) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + suffix
}

func tildeFenceForContent(s string) string {
	maxRun := 0
	cur := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '~' {
			cur++
			if cur > maxRun {
				maxRun = cur
			}
		} else {
			cur = 0
		}
	}
	fenceLen := 4
	if maxRun >= fenceLen {
		fenceLen = maxRun + 1
	}
	return strings.Repeat("~", fenceLen)
}
