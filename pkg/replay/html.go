package replay

import (
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/redact"
)

type htmlCheckpointDiff struct {
	PrevStep    int
	FromHash    string
	ToHash      string
	IsInitial   bool
	InitialList []string
	FilesAdded  int
	FilesMod    int
	FilesDel    int
	Changes     []FileDiffEntry
}

// renderHTMLTimelineReport builds a self-contained, zero-CDN, dark-mode interactive HTML
// timeline report for session. Every untrusted string is scrubbed with pkg/redact and
// escaped with html.EscapeString before rendering, and the embedded vanilla JS operates
// strictly via DOM class/attribute toggles (zero innerHTML).
func (e *Engine) renderHTMLTimelineReport(session *model.Session) string {
	// Always scrub all session text (command, workspace dir, frames, tool args/results)
	// before rendering HTML so credentials never leak into shareable reports.
	clean := redact.Session(session)
	if clean == nil {
		clean = &model.Session{}
	}

	diffByStep := e.computeConsecutiveCheckpointDiffs(clean)

	counts := map[string]int{
		"all":        len(clean.Frames),
		"checkpoint": 0,
		"output":     0,
		"error":      0,
		"tool_call":  0,
	}
	for _, f := range clean.Frames {
		switch f.Type {
		case model.EventCheckpoint:
			counts["checkpoint"]++
		case model.EventOutput:
			counts["output"]++
		case model.EventError:
			counts["error"]++
		case model.EventToolCall, model.EventToolResult:
			counts["tool_call"]++
		}
	}

	durationStr := "active"
	endedStr := "active"
	if !clean.EndTime.IsZero() {
		endedStr = clean.EndTime.UTC().Format(time.RFC3339)
		durationStr = clean.EndTime.Sub(clean.StartTime).Round(time.Millisecond).String()
	}
	startedStr := clean.StartTime.UTC().Format(time.RFC3339)

	statusClass := "status-running"
	switch {
	case clean.Status == "completed":
		statusClass = "status-ok"
	case clean.Status == "aborted":
		statusClass = "status-err"
	case strings.HasPrefix(clean.Status, "rewound"):
		statusClass = "status-rewound"
	}

	var sb strings.Builder
	sb.Grow(16 * 1024)

	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TracePort Timeline — `)
	sb.WriteString(html.EscapeString(clean.ID))
	sb.WriteString(`</title>
<style>
:root {
  --crust: #11111b;
  --mantle: #181825;
  --base: #1e1e2e;
  --surface0: #313244;
  --surface1: #45475a;
  --overlay0: #6c7086;
  --subtext: #a6adc8;
  --text: #cdd6f4;
  --blue: #89b4fa;
  --lavender: #b4befe;
  --sapphire: #74c7ec;
  --teal: #94e2d5;
  --green: #a6e3a1;
  --yellow: #f9e2af;
  --peach: #fab387;
  --red: #f38ba8;
  --mauve: #cba6f7;
  --mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace;
  --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Oxygen, Ubuntu, sans-serif;
}
* { box-sizing: border-box; }
html, body {
  margin: 0;
  padding: 0;
  background: var(--crust);
  color: var(--text);
  font-family: var(--sans);
  font-size: 14px;
  line-height: 1.5;
  height: 100%;
}
body {
  display: flex;
  flex-direction: column;
  min-height: 100vh;
}
header.tp-header {
  background: var(--mantle);
  border-bottom: 1px solid var(--surface0);
  padding: 16px 22px;
}
.tp-brand-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 10px;
}
.tp-brand {
  display: flex;
  align-items: center;
  gap: 10px;
}
.tp-logo {
  font-family: var(--mono);
  font-weight: 700;
  letter-spacing: 0.08em;
  font-size: 13px;
  color: var(--crust);
  background: linear-gradient(135deg, var(--blue), var(--mauve));
  padding: 3px 9px;
  border-radius: 6px;
}
.tp-session-id {
  font-family: var(--mono);
  font-size: 16px;
  font-weight: 600;
  color: var(--text);
}
.tp-badges {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-family: var(--mono);
  font-size: 11px;
  font-weight: 600;
  padding: 3px 9px;
  border-radius: 999px;
  border: 1px solid var(--surface1);
  background: var(--base);
  color: var(--subtext);
}
.status-ok { color: var(--green); border-color: rgba(166, 227, 161, 0.35); background: rgba(166, 227, 161, 0.1); }
.status-err { color: var(--red); border-color: rgba(243, 139, 168, 0.35); background: rgba(243, 139, 168, 0.1); }
.status-running { color: var(--yellow); border-color: rgba(249, 226, 175, 0.35); background: rgba(249, 226, 175, 0.1); }
.status-rewound { color: var(--mauve); border-color: rgba(203, 166, 247, 0.35); background: rgba(203, 166, 247, 0.1); }
.badge-sec { color: var(--teal); border-color: rgba(148, 226, 213, 0.3); background: rgba(148, 226, 213, 0.08); }
.badge-tag { color: var(--lavender); border-color: rgba(180, 190, 254, 0.3); background: rgba(180, 190, 254, 0.08); }
.tp-meta-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
  gap: 8px 16px;
  background: var(--base);
  border: 1px solid var(--surface0);
  border-radius: 8px;
  padding: 10px 14px;
}
.tp-meta-item {
  min-width: 0;
}
.tp-meta-label {
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--overlay0);
}
.tp-meta-val {
  font-family: var(--mono);
  font-size: 12.5px;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tp-cmd-bar {
  margin-top: 8px;
  font-family: var(--mono);
  font-size: 12.5px;
  background: var(--crust);
  border: 1px solid var(--surface0);
  border-radius: 6px;
  padding: 7px 11px;
  color: var(--blue);
  overflow-x: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.tp-layout {
  display: grid;
  grid-template-columns: 340px 1fr;
  flex: 1;
  min-height: 0;
}
aside.tp-sidebar {
  background: var(--mantle);
  border-right: 1px solid var(--surface0);
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.tp-filters {
  padding: 12px;
  border-bottom: 1px solid var(--surface0);
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.tp-filter-row {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.filter-btn {
  appearance: none;
  border: 1px solid var(--surface1);
  background: var(--base);
  color: var(--subtext);
  font-family: var(--mono);
  font-size: 11.5px;
  padding: 4px 9px;
  border-radius: 6px;
  cursor: pointer;
  transition: background 0.12s ease, border-color 0.12s ease, color 0.12s ease;
}
.filter-btn:hover {
  border-color: var(--blue);
  color: var(--text);
}
.filter-btn.active {
  background: rgba(137, 180, 250, 0.16);
  border-color: var(--blue);
  color: var(--blue);
  font-weight: 600;
}
.tp-search {
  width: 100%;
  background: var(--crust);
  border: 1px solid var(--surface1);
  border-radius: 6px;
  padding: 6px 10px;
  color: var(--text);
  font-family: var(--mono);
  font-size: 12px;
  outline: none;
}
.tp-search:focus { border-color: var(--blue); }
.tp-step-list {
  overflow-y: auto;
  flex: 1;
  padding: 8px;
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.step-item {
  width: 100%;
  text-align: left;
  background: var(--base);
  border: 1px solid var(--surface0);
  border-radius: 7px;
  padding: 8px 10px;
  color: var(--text);
  cursor: pointer;
  display: flex;
  flex-direction: column;
  gap: 4px;
  transition: border-color 0.12s ease, background 0.12s ease;
}
.step-item:hover { border-color: var(--surface1); background: #242436; }
.step-item.active {
  border-color: var(--blue);
  background: rgba(137, 180, 250, 0.12);
}
.step-item[hidden] { display: none; }
.step-row-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
  font-family: var(--mono);
  font-size: 11.5px;
}
.step-num { font-weight: 700; color: var(--lavender); }
.step-ts { color: var(--overlay0); font-size: 11px; }
.type-pill {
  font-family: var(--mono);
  font-size: 10.5px;
  font-weight: 600;
  padding: 1px 6px;
  border-radius: 4px;
  text-transform: uppercase;
}
.type-checkpoint { background: rgba(203, 166, 247, 0.18); color: var(--mauve); }
.type-output { background: rgba(137, 180, 250, 0.18); color: var(--blue); }
.type-error { background: rgba(243, 139, 168, 0.18); color: var(--red); }
.type-tool_call, .type-tool_result { background: rgba(249, 226, 175, 0.18); color: var(--yellow); }
.type-input { background: rgba(148, 226, 213, 0.18); color: var(--teal); }
.step-preview {
  font-family: var(--mono);
  font-size: 11.5px;
  color: var(--subtext);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.step-diff-pills {
  display: flex;
  gap: 6px;
  font-family: var(--mono);
  font-size: 10.5px;
}
.pill-add { color: var(--green); }
.pill-mod { color: var(--yellow); }
.pill-del { color: var(--red); }
main.tp-inspector {
  background: var(--base);
  overflow-y: auto;
  padding: 20px 24px;
}
.step-panel[hidden] { display: none; }
.panel-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--surface0);
  margin-bottom: 16px;
}
.panel-title {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 17px;
  font-weight: 600;
}
.panel-meta {
  font-family: var(--mono);
  font-size: 12px;
  color: var(--subtext);
}
.card {
  background: var(--mantle);
  border: 1px solid var(--surface0);
  border-radius: 8px;
  margin-bottom: 16px;
  overflow: hidden;
}
.card-head {
  background: rgba(49, 50, 68, 0.45);
  border-bottom: 1px solid var(--surface0);
  padding: 8px 13px;
  font-family: var(--mono);
  font-size: 12px;
  font-weight: 600;
  color: var(--subtext);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
pre.code-block {
  margin: 0;
  padding: 13px 15px;
  font-family: var(--mono);
  font-size: 12.5px;
  line-height: 1.55;
  color: var(--text);
  overflow-x: auto;
  white-space: pre-wrap;
  word-break: break-word;
}
pre.code-error {
  color: var(--red);
  background: rgba(243, 139, 168, 0.06);
}
.diff-file-block {
  border-bottom: 1px solid var(--surface0);
}
.diff-file-block:last-child { border-bottom: none; }
.diff-file-bar {
  padding: 7px 13px;
  background: var(--crust);
  font-family: var(--mono);
  font-size: 12px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  border-bottom: 1px solid var(--surface0);
}
.diff-status-added { color: var(--green); }
.diff-status-modified { color: var(--yellow); }
.diff-status-deleted { color: var(--red); }
.diff-body {
  font-family: var(--mono);
  font-size: 12px;
  line-height: 1.5;
  overflow-x: auto;
  padding: 6px 0;
}
.diff-line {
  padding: 1px 14px;
  white-space: pre;
}
.diff-add {
  background: rgba(166, 227, 161, 0.13);
  color: var(--green);
}
.diff-del {
  background: rgba(243, 139, 168, 0.13);
  color: var(--red);
}
.diff-hunk {
  background: rgba(148, 226, 213, 0.10);
  color: var(--teal);
  font-weight: 600;
}
.diff-hdr {
  color: var(--sapphire);
  font-weight: 600;
}
.diff-ctx {
  color: var(--subtext);
}
.empty-state {
  padding: 32px;
  text-align: center;
  color: var(--overlay0);
  font-family: var(--mono);
}
@media (max-width: 860px) {
  .tp-layout { grid-template-columns: 1fr; }
  aside.tp-sidebar { max-height: 42vh; border-right: none; border-bottom: 1px solid var(--surface0); }
}
</style>
</head>
<body>
`)

	// Header
	sb.WriteString("<header class=\"tp-header\">\n")
	sb.WriteString("  <div class=\"tp-brand-row\">\n")
	sb.WriteString("    <div class=\"tp-brand\">\n")
	sb.WriteString("      <span class=\"tp-logo\">TRACEPORT</span>\n")
	sb.WriteString("      <span class=\"tp-session-id\">")
	sb.WriteString(html.EscapeString(clean.ID))
	sb.WriteString("</span>\n")
	sb.WriteString("    </div>\n")
	sb.WriteString("    <div class=\"tp-badges\">\n")
	sb.WriteString(fmt.Sprintf("      <span class=\"badge %s\">status: %s</span>\n", statusClass, html.EscapeString(clean.Status)))
	sb.WriteString(fmt.Sprintf("      <span class=\"badge\">exit: %d</span>\n", clean.ExitCode))
	for _, tag := range clean.Tags {
		sb.WriteString(fmt.Sprintf("      <span class=\"badge badge-tag\">#%s</span>\n", html.EscapeString(tag)))
	}
	sb.WriteString("      <span class=\"badge badge-sec\">redacted &amp; self-contained</span>\n")
	sb.WriteString("    </div>\n")
	sb.WriteString("  </div>\n")

	sb.WriteString("  <div class=\"tp-meta-grid\">\n")
	sb.WriteString(fmt.Sprintf("    <div class=\"tp-meta-item\"><div class=\"tp-meta-label\">Started (UTC)</div><div class=\"tp-meta-val\">%s</div></div>\n", html.EscapeString(startedStr)))
	sb.WriteString(fmt.Sprintf("    <div class=\"tp-meta-item\"><div class=\"tp-meta-label\">Ended (UTC)</div><div class=\"tp-meta-val\">%s</div></div>\n", html.EscapeString(endedStr)))
	sb.WriteString(fmt.Sprintf("    <div class=\"tp-meta-item\"><div class=\"tp-meta-label\">Duration</div><div class=\"tp-meta-val\">%s</div></div>\n", html.EscapeString(durationStr)))
	sb.WriteString(fmt.Sprintf("    <div class=\"tp-meta-item\"><div class=\"tp-meta-label\">Frames / Checkpoints</div><div class=\"tp-meta-val\">%d / %d</div></div>\n", len(clean.Frames), counts["checkpoint"]))
	if clean.WorkspaceDir != "" {
		sb.WriteString(fmt.Sprintf("    <div class=\"tp-meta-item\"><div class=\"tp-meta-label\">Workspace</div><div class=\"tp-meta-val\" title=\"%s\">%s</div></div>\n", html.EscapeString(clean.WorkspaceDir), html.EscapeString(clean.WorkspaceDir)))
	}
	sb.WriteString("  </div>\n")

	sb.WriteString("  <div class=\"tp-cmd-bar\">$ ")
	sb.WriteString(html.EscapeString(clean.Command))
	sb.WriteString("</div>\n")
	sb.WriteString("</header>\n")

	// Main 2-pane interactive layout
	sb.WriteString("<div class=\"tp-layout\">\n")

	// Left Sidebar: filter buttons + clickable timeline
	sb.WriteString("  <aside class=\"tp-sidebar\">\n")
	sb.WriteString("    <div class=\"tp-filters\">\n")
	sb.WriteString("      <div class=\"tp-filter-row\" role=\"group\" aria-label=\"Filter timeline steps\">\n")
	filterDefs := []struct {
		key   string
		label string
	}{
		{"all", "All"},
		{"checkpoint", "Checkpoint"},
		{"output", "Output"},
		{"error", "Error"},
		{"tool_call", "Tool Call"},
	}
	for i, fd := range filterDefs {
		activeCls := ""
		if i == 0 {
			activeCls = " active"
		}
		sb.WriteString(fmt.Sprintf(
			"        <button type=\"button\" class=\"filter-btn%s\" data-filter=\"%s\">%s (%d)</button>\n",
			activeCls,
			fd.key,
			fd.label,
			counts[fd.key],
		))
	}
	sb.WriteString("      </div>\n")
	sb.WriteString("      <input id=\"tp-search\" class=\"tp-search\" type=\"search\" placeholder=\"Filter steps by text or path...\" aria-label=\"Filter steps\">\n")
	sb.WriteString("    </div>\n")

	sb.WriteString("    <div class=\"tp-step-list\" id=\"tp-step-list\">\n")
	if len(clean.Frames) == 0 {
		sb.WriteString("      <div class=\"empty-state\">No frames recorded in this session.</div>\n")
	} else {
		for idx, f := range clean.Frames {
			stepNum := f.Index
			if stepNum <= 0 {
				stepNum = idx + 1
			}
			filterGroup := string(f.Type)
			if f.Type == model.EventToolResult {
				filterGroup = "tool_call"
			}
			activeCls := ""
			if idx == 0 {
				activeCls = " active"
			}
			preview := compactHTMLPreview(f.Content, 64)
			if preview == "" && f.Tool != nil {
				preview = "tool: " + f.Tool.Name
			}
			ts := f.Timestamp.UTC().Format("15:04:05.000")

			sb.WriteString(fmt.Sprintf(
				"      <button type=\"button\" class=\"step-item%s\" data-step=\"%d\" data-type=\"%s\">\n",
				activeCls,
				stepNum,
				html.EscapeString(filterGroup),
			))
			sb.WriteString("        <div class=\"step-row-top\">\n")
			sb.WriteString(fmt.Sprintf("          <span><span class=\"step-num\">#%d</span> <span class=\"type-pill type-%s\">%s</span></span>\n",
				stepNum, html.EscapeString(string(f.Type)), html.EscapeString(string(f.Type))))
			sb.WriteString(fmt.Sprintf("          <span class=\"step-ts\">%s</span>\n", html.EscapeString(ts)))
			sb.WriteString("        </div>\n")
			sb.WriteString(fmt.Sprintf("        <div class=\"step-preview\">%s</div>\n", html.EscapeString(preview)))
			if d, hasDiff := diffByStep[stepNum]; hasDiff {
				if d.IsInitial {
					sb.WriteString(fmt.Sprintf("        <div class=\"step-diff-pills\"><span>initial: %d files</span></div>\n", len(d.InitialList)))
				} else if d.FilesAdded+d.FilesMod+d.FilesDel > 0 {
					sb.WriteString(fmt.Sprintf(
						"        <div class=\"step-diff-pills\"><span class=\"pill-add\">+%d</span><span class=\"pill-mod\">~%d</span><span class=\"pill-del\">-%d</span></div>\n",
						d.FilesAdded, d.FilesMod, d.FilesDel,
					))
				}
			}
			sb.WriteString("      </button>\n")
		}
	}
	sb.WriteString("    </div>\n")
	sb.WriteString("  </aside>\n")

	// Right Inspector Pane: pre-rendered, HTML-escaped step panels
	sb.WriteString("  <main class=\"tp-inspector\" id=\"tp-inspector\">\n")
	if len(clean.Frames) == 0 {
		sb.WriteString("    <div class=\"empty-state\">Select a recorded step to inspect output and diffs.</div>\n")
	} else {
		for idx, f := range clean.Frames {
			stepNum := f.Index
			if stepNum <= 0 {
				stepNum = idx + 1
			}
			hiddenAttr := ""
			if idx > 0 {
				hiddenAttr = " hidden"
			}
			sb.WriteString(fmt.Sprintf("    <section class=\"step-panel\" data-step=\"%d\"%s>\n", stepNum, hiddenAttr))
			sb.WriteString("      <div class=\"panel-head\">\n")
			sb.WriteString(fmt.Sprintf("        <div class=\"panel-title\"><span>Step #%d</span> <span class=\"type-pill type-%s\">%s</span></div>\n",
				stepNum, html.EscapeString(string(f.Type)), html.EscapeString(string(f.Type))))
			sb.WriteString(fmt.Sprintf("        <div class=\"panel-meta\">%s", html.EscapeString(f.Timestamp.UTC().Format(time.RFC3339Nano))))
			if f.GitSnapshot != "" {
				shortHash := f.GitSnapshot
				if len(shortHash) > 12 {
					shortHash = shortHash[:12]
				}
				sb.WriteString(fmt.Sprintf(" &middot; tree:%s", html.EscapeString(shortHash)))
			}
			sb.WriteString("</div>\n")
			sb.WriteString("      </div>\n")

			// Content / stdout / stderr block
			contentLabel := "Frame Output"
			preClass := "code-block"
			switch f.Type {
			case model.EventError:
				contentLabel = "Stderr / Error Output (Redacted)"
				preClass = "code-block code-error"
			case model.EventOutput:
				contentLabel = "Stdout / Agent Output (Redacted)"
			case model.EventCheckpoint:
				contentLabel = "Checkpoint Label"
			case model.EventInput:
				contentLabel = "Input / Prompt (Redacted)"
			case model.EventToolCall:
				contentLabel = "Tool Call Message (Redacted)"
			case model.EventToolResult:
				contentLabel = "Tool Result Message (Redacted)"
			}
			sb.WriteString("      <div class=\"card\">\n")
			sb.WriteString(fmt.Sprintf("        <div class=\"card-head\"><span>%s</span></div>\n", contentLabel))
			bodyText := f.Content
			if bodyText == "" {
				bodyText = "(empty)"
			}
			sb.WriteString(fmt.Sprintf("        <pre class=\"%s\">%s</pre>\n", preClass, html.EscapeString(bodyText)))
			sb.WriteString("      </div>\n")

			// Tool invocation details
			if f.Tool != nil {
				sb.WriteString("      <div class=\"card\">\n")
				durMeta := ""
				if f.Tool.DurationMs > 0 {
					durMeta = fmt.Sprintf("<span>%d ms</span>", f.Tool.DurationMs)
				}
				sb.WriteString(fmt.Sprintf("        <div class=\"card-head\"><span>Tool Invocation: %s</span>%s</div>\n", html.EscapeString(f.Tool.Name), durMeta))
				if len(f.Tool.Arguments) > 0 {
					if rawArgs, err := json.MarshalIndent(f.Tool.Arguments, "", "  "); err == nil {
						sb.WriteString(fmt.Sprintf("        <pre class=\"code-block\">%s</pre>\n", html.EscapeString(string(rawArgs))))
					}
				}
				if strings.TrimSpace(f.Tool.Result) != "" {
					sb.WriteString("        <div class=\"card-head\"><span>Tool Result</span></div>\n")
					sb.WriteString(fmt.Sprintf("        <pre class=\"code-block\">%s</pre>\n", html.EscapeString(f.Tool.Result)))
				}
				sb.WriteString("      </div>\n")
			}

			// Checkpoint unified diff card
			if cpDiff, ok := diffByStep[stepNum]; ok {
				renderHTMLCheckpointDiffCard(&sb, cpDiff)
			}

			sb.WriteString("    </section>\n")
		}
	}
	sb.WriteString("  </main>\n")
	sb.WriteString("</div>\n")

	// Embedded vanilla JS — zero external dependencies, zero innerHTML.
	sb.WriteString(`<script>
(function () {
  var activeFilter = "all";
  var searchQuery = "";
  var filterButtons = Array.prototype.slice.call(document.querySelectorAll(".filter-btn"));
  var stepItems = Array.prototype.slice.call(document.querySelectorAll(".step-item"));
  var stepPanels = Array.prototype.slice.call(document.querySelectorAll(".step-panel"));
  var searchInput = document.getElementById("tp-search");

  function selectStep(stepNum) {
    stepItems.forEach(function (btn) {
      if (btn.getAttribute("data-step") === String(stepNum)) {
        btn.classList.add("active");
      } else {
        btn.classList.remove("active");
      }
    });
    stepPanels.forEach(function (panel) {
      if (panel.getAttribute("data-step") === String(stepNum)) {
        panel.removeAttribute("hidden");
      } else {
        panel.setAttribute("hidden", "");
      }
    });
  }

  function applyFilters() {
    var firstVisible = null;
    var activeStillVisible = false;
    stepItems.forEach(function (btn) {
      var kind = btn.getAttribute("data-type") || "";
      var matchesFilter = (activeFilter === "all") || (kind === activeFilter);
      var text = (btn.textContent || "").toLowerCase();
      var matchesSearch = !searchQuery || text.indexOf(searchQuery) !== -1;
      if (matchesFilter && matchesSearch) {
        btn.removeAttribute("hidden");
        if (!firstVisible) { firstVisible = btn; }
        if (btn.classList.contains("active")) { activeStillVisible = true; }
      } else {
        btn.setAttribute("hidden", "");
      }
    });
    if (!activeStillVisible && firstVisible) {
      selectStep(firstVisible.getAttribute("data-step"));
    }
  }

  filterButtons.forEach(function (btn) {
    btn.addEventListener("click", function () {
      activeFilter = btn.getAttribute("data-filter") || "all";
      filterButtons.forEach(function (b) { b.classList.remove("active"); });
      btn.classList.add("active");
      applyFilters();
    });
  });

  stepItems.forEach(function (btn) {
    btn.addEventListener("click", function () {
      selectStep(btn.getAttribute("data-step"));
    });
  });

  if (searchInput) {
    searchInput.addEventListener("input", function () {
      searchQuery = (searchInput.value || "").trim().toLowerCase();
      applyFilters();
    });
  }

  document.addEventListener("keydown", function (ev) {
    if (document.activeElement === searchInput) { return; }
    if (ev.key !== "ArrowDown" && ev.key !== "ArrowUp" && ev.key !== "j" && ev.key !== "k") { return; }
    var visible = stepItems.filter(function (b) { return !b.hasAttribute("hidden"); });
    if (!visible.length) { return; }
    var curIdx = visible.findIndex(function (b) { return b.classList.contains("active"); });
    if (curIdx < 0) { curIdx = 0; }
    if (ev.key === "ArrowDown" || ev.key === "j") {
      curIdx = Math.min(visible.length - 1, curIdx + 1);
    } else {
      curIdx = Math.max(0, curIdx - 1);
    }
    ev.preventDefault();
    selectStep(visible[curIdx].getAttribute("data-step"));
    visible[curIdx].scrollIntoView({ block: "nearest" });
  });
})();
</script>
</body>
</html>
`)

	return sb.String()
}

func (e *Engine) computeConsecutiveCheckpointDiffs(session *model.Session) map[int]htmlCheckpointDiff {
	out := make(map[int]htmlCheckpointDiff)
	if session == nil || len(session.Frames) == 0 || e.store == nil {
		return out
	}

	treeStore, err := checkpoint.NewTreeStore(e.store.RootDir(), e.store.RootDir())
	if err != nil {
		return out
	}
	defer treeStore.Close()

	var prevStep int
	var prevHash string
	var prevManifest *checkpoint.TreeManifest

	for idx, f := range session.Frames {
		if f.GitSnapshot == "" || !treeStore.HasTree(f.GitSnapshot) {
			continue
		}
		stepNum := f.Index
		if stepNum <= 0 {
			stepNum = idx + 1
		}
		curManifest, mErr := treeStore.LoadManifest(f.GitSnapshot)
		if mErr != nil {
			continue
		}

		if prevManifest == nil {
			paths := make([]string, 0, len(curManifest.Files))
			for _, fe := range curManifest.Files {
				paths = append(paths, redact.String(fe.Path))
			}
			sort.Strings(paths)
			out[stepNum] = htmlCheckpointDiff{
				ToHash:      f.GitSnapshot,
				IsInitial:   true,
				InitialList: paths,
			}
		} else {
			diffEntry := computeManifestPairDiff(treeStore, prevStep, prevHash, f.GitSnapshot, prevManifest, curManifest)
			out[stepNum] = diffEntry
		}

		prevStep = stepNum
		prevHash = f.GitSnapshot
		prevManifest = curManifest
	}
	return out
}

func computeManifestPairDiff(
	treeStore *checkpoint.TreeStore,
	prevStep int,
	fromHash, toHash string,
	fromManifest, toManifest *checkpoint.TreeManifest,
) htmlCheckpointDiff {
	res := htmlCheckpointDiff{
		PrevStep: prevStep,
		FromHash: fromHash,
		ToHash:   toHash,
	}
	if fromHash == toHash {
		return res
	}

	fromMap := make(map[string]checkpoint.FileEntry, len(fromManifest.Files))
	for _, f := range fromManifest.Files {
		fromMap[f.Path] = f
	}
	toMap := make(map[string]checkpoint.FileEntry, len(toManifest.Files))
	for _, f := range toManifest.Files {
		toMap[f.Path] = f
	}

	allSet := make(map[string]struct{}, len(fromMap)+len(toMap))
	for p := range fromMap {
		allSet[p] = struct{}{}
	}
	for p := range toMap {
		allSet[p] = struct{}{}
	}
	allPaths := make([]string, 0, len(allSet))
	for p := range allSet {
		allPaths = append(allPaths, p)
	}
	sort.Strings(allPaths)

	for _, p := range allPaths {
		a, inA := fromMap[p]
		b, inB := toMap[p]
		if inA && inB && a.SHA256 == b.SHA256 {
			continue
		}
		safePath := redact.String(p)
		switch {
		case !inA && inB:
			bBytes, err := treeStore.ReadBlob(b.SHA256)
			if err != nil {
				continue
			}
			u := redact.String(formatFileUnifiedDiff("/dev/null", "b/"+safePath, nil, bBytes))
			res.FilesAdded++
			res.Changes = append(res.Changes, FileDiffEntry{Path: safePath, Status: "added", Unified: u})
		case inA && !inB:
			aBytes, err := treeStore.ReadBlob(a.SHA256)
			if err != nil {
				continue
			}
			u := redact.String(formatFileUnifiedDiff("a/"+safePath, "/dev/null", aBytes, nil))
			res.FilesDel++
			res.Changes = append(res.Changes, FileDiffEntry{Path: safePath, Status: "deleted", Unified: u})
		default:
			aBytes, errA := treeStore.ReadBlob(a.SHA256)
			bBytes, errB := treeStore.ReadBlob(b.SHA256)
			if errA != nil || errB != nil {
				continue
			}
			u := redact.String(formatFileUnifiedDiff("a/"+safePath, "b/"+safePath, aBytes, bBytes))
			res.FilesMod++
			res.Changes = append(res.Changes, FileDiffEntry{Path: safePath, Status: "modified", Unified: u})
		}
	}
	return res
}

func renderHTMLCheckpointDiffCard(sb *strings.Builder, d htmlCheckpointDiff) {
	sb.WriteString("      <div class=\"card\">\n")
	if d.IsInitial {
		sb.WriteString(fmt.Sprintf("        <div class=\"card-head\"><span>Initial Workspace Baseline Snapshot</span><span>%d tracked files</span></div>\n", len(d.InitialList)))
		if len(d.InitialList) == 0 {
			sb.WriteString("        <pre class=\"code-block\">(empty workspace)</pre>\n")
		} else {
			shown := d.InitialList
			extra := 0
			if len(shown) > 200 {
				extra = len(shown) - 200
				shown = shown[:200]
			}
			var listBuf strings.Builder
			for _, p := range shown {
				listBuf.WriteString(p)
				listBuf.WriteByte('\n')
			}
			if extra > 0 {
				listBuf.WriteString(fmt.Sprintf("... and %d more files\n", extra))
			}
			sb.WriteString(fmt.Sprintf("        <pre class=\"code-block\">%s</pre>\n", html.EscapeString(listBuf.String())))
		}
		sb.WriteString("      </div>\n")
		return
	}

	sb.WriteString(fmt.Sprintf(
		"        <div class=\"card-head\"><span>Unified Diff vs Step #%d (Redacted)</span><span><span class=\"pill-add\">+%d added</span> &middot; <span class=\"pill-mod\">~%d modified</span> &middot; <span class=\"pill-del\">-%d deleted</span></span></div>\n",
		d.PrevStep, d.FilesAdded, d.FilesMod, d.FilesDel,
	))

	if len(d.Changes) == 0 {
		sb.WriteString("        <pre class=\"code-block\">No filesystem changes since previous checkpoint.</pre>\n")
		sb.WriteString("      </div>\n")
		return
	}

	for _, ch := range d.Changes {
		sb.WriteString("        <div class=\"diff-file-block\">\n")
		sb.WriteString(fmt.Sprintf(
			"          <div class=\"diff-file-bar\"><span>%s</span><span class=\"diff-status-%s\">%s</span></div>\n",
			html.EscapeString(ch.Path),
			html.EscapeString(ch.Status),
			html.EscapeString(ch.Status),
		))
		sb.WriteString("          <div class=\"diff-body\">\n")
		lines := strings.Split(strings.TrimSuffix(ch.Unified, "\n"), "\n")
		for _, line := range lines {
			cls := "diff-ctx"
			switch {
			case strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- "):
				cls = "diff-hdr"
			case strings.HasPrefix(line, "@@"):
				cls = "diff-hunk"
			case strings.HasPrefix(line, "+"):
				cls = "diff-add"
			case strings.HasPrefix(line, "-"):
				cls = "diff-del"
			}
			sb.WriteString(fmt.Sprintf("            <div class=\"diff-line %s\">%s</div>\n", cls, html.EscapeString(line)))
		}
		sb.WriteString("          </div>\n")
		sb.WriteString("        </div>\n")
	}

	sb.WriteString("      </div>\n")
}

func compactHTMLPreview(s string, maxRunes int) string {
	clean := strings.ReplaceAll(s, "\r", "")
	clean = strings.ReplaceAll(clean, "\n", " ")
	clean = strings.TrimSpace(clean)
	return truncateUTF8RunesWithSuffix(clean, maxRunes, "...")
}
