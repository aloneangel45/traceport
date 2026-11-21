package checkpoint

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var defaultWatchIgnoredDirs = map[string]bool{
	".git":        true,
	".traceport":  true,
	"node_modules": true,
	"target":      true,
	".next":       true,
	"__pycache__": true,
	".venv":       true,
	"venv":        true,
}

type ignoreRule struct {
	pattern  string
	negated  bool
	dirOnly  bool
	anchored bool
	hasSlash bool
}

// IgnoreMatcher evaluates .gitignore, .traceportignore, and default cache/dependency
// directory rules against workspace-relative slash paths.
type IgnoreMatcher struct {
	includeDefaultDirs bool
	rules              []ignoreRule
	hasNegation        bool
}

// LoadIgnoreMatcher loads ignore rules from <workRoot>/.gitignore and
// <workRoot>/.traceportignore (if present). When includeDefaultDirs is true,
// heavy build/dependency directories (node_modules, target, .next, __pycache__, .venv, venv)
// are always ignored alongside .git and .traceport.
func LoadIgnoreMatcher(workRoot string, includeDefaultDirs bool) *IgnoreMatcher {
	m := &IgnoreMatcher{
		includeDefaultDirs: includeDefaultDirs,
	}
	for _, name := range []string{".gitignore", ".traceportignore"} {
		m.loadFile(filepath.Join(workRoot, name))
	}
	return m
}

func (m *IgnoreMatcher) loadFile(filePath string) {
	f, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	firstLine := true
	for scanner.Scan() {
		rawLine := scanner.Text()
		if firstLine {
			firstLine = false
			rawLine = strings.TrimPrefix(rawLine, "\uFEFF")
		}
		line := strings.TrimRight(rawLine, "\r")
		pat := trimIgnoreSpaces(line)
		if pat == "" || strings.HasPrefix(pat, "#") {
			continue
		}

		rule := ignoreRule{}
		if strings.HasPrefix(pat, `\!`) || strings.HasPrefix(pat, `\#`) {
			pat = pat[1:]
		} else if strings.HasPrefix(pat, "!") {
			rule.negated = true
			m.hasNegation = true
			pat = strings.TrimPrefix(pat, "!")
			if pat == "" {
				continue
			}
		}

		pat = normalizeIgnorePatternSlashes(pat)

		if strings.HasSuffix(pat, "/") {
			rule.dirOnly = true
			pat = strings.TrimRight(pat, "/")
			if pat == "" {
				continue
			}
		}

		if strings.HasPrefix(pat, "/") {
			rule.anchored = true
			pat = strings.TrimLeft(pat, "/")
			if pat == "" {
				continue
			}
		}

		if strings.Contains(pat, "/") {
			rule.hasSlash = true
		}

		rule.pattern = pat
		m.rules = append(m.rules, rule)
	}
}

// trimIgnoreSpaces removes leading whitespace and unescaped trailing whitespace
// while preserving backslash-escaped trailing spaces (e.g. "foo\ ").
func trimIgnoreSpaces(line string) string {
	s := strings.TrimLeft(line, " \t")
	for len(s) > 0 {
		last := s[len(s)-1]
		if last != ' ' && last != '\t' {
			break
		}
		bs := 0
		for i := len(s) - 2; i >= 0 && s[i] == '\\'; i-- {
			bs++
		}
		if bs%2 == 1 {
			// Escaped trailing space: unescape the final "\ " and preserve it.
			s = s[:len(s)-2] + string(last)
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// normalizeIgnorePatternSlashes converts Windows path backslashes inside ignore rules
// to forward slashes while preserving glob/gitignore backslash escapes.
func normalizeIgnorePatternSlashes(pat string) string {
	if !strings.ContainsRune(pat, '\\') {
		return pat
	}
	var sb strings.Builder
	sb.Grow(len(pat))
	for i := 0; i < len(pat); i++ {
		if pat[i] == '\\' && i+1 < len(pat) {
			next := pat[i+1]
			switch next {
			case '*', '?', '[', ']', '\\', '!', '#', ' ':
				sb.WriteByte(pat[i])
				sb.WriteByte(next)
				i++
				continue
			default:
				sb.WriteByte('/')
				continue
			}
		}
		if pat[i] == '\\' && i == len(pat)-1 {
			sb.WriteByte('/')
			continue
		}
		sb.WriteByte(pat[i])
	}
	return sb.String()
}

func normalizeIgnoreRel(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	for strings.HasPrefix(rel, "./") {
		rel = strings.TrimPrefix(rel, "./")
	}
	rel = strings.TrimLeft(rel, "/")
	rel = strings.TrimRight(rel, "/")
	return rel
}

// ShouldIgnore reports whether rel (slash-separated or Windows backslash-separated path
// relative to workspace root) should be excluded. isDir indicates whether rel is a directory.
func (m *IgnoreMatcher) ShouldIgnore(rel string, isDir bool) bool {
	if m == nil {
		return false
	}
	rel = normalizeIgnoreRel(rel)
	if rel == "" || rel == "." {
		return false
	}

	parts := strings.Split(rel, "/")
	base := parts[len(parts)-1]

	if m.includeDefaultDirs {
		for i, seg := range parts {
			if (i < len(parts)-1 || isDir) && defaultWatchIgnoredDirs[seg] {
				return true
			}
		}
	}

	ignored := false
	for _, rule := range m.rules {
		if rule.matches(rel, base, isDir) {
			ignored = !rule.negated
		}
	}
	return ignored
}

// CanSkipDir reports whether filepath.WalkDir can safely return filepath.SkipDir for rel.
func (m *IgnoreMatcher) CanSkipDir(rel string) bool {
	if m == nil {
		return false
	}
	rel = normalizeIgnoreRel(rel)
	if rel == "" || rel == "." {
		return false
	}
	parts := strings.Split(rel, "/")
	if m.includeDefaultDirs {
		for _, seg := range parts {
			if defaultWatchIgnoredDirs[seg] {
				return true
			}
		}
	}
	if !m.ShouldIgnore(rel, true) {
		return false
	}
	// If any negation rule exists, only skip if no negation rule could match inside this directory.
	if m.hasNegation {
		for _, r := range m.rules {
			if !r.negated {
				continue
			}
			if canNegationMatchUnderDir(r, rel) {
				return false
			}
		}
	}
	return true
}

func canNegationMatchUnderDir(r ignoreRule, dirRel string) bool {
	if !r.anchored && !r.hasSlash {
		return true
	}
	patParts := strings.Split(r.pattern, "/")
	dirParts := strings.Split(dirRel, "/")
	for i := 0; i < len(dirParts); i++ {
		if i >= len(patParts) {
			return false
		}
		if patParts[i] == "**" {
			return true
		}
		ok, err := path.Match(patParts[i], dirParts[i])
		if err != nil || !ok {
			return false
		}
	}
	return len(patParts) > len(dirParts)
}

func (r ignoreRule) matches(rel, base string, isDir bool) bool {
	if r.dirOnly {
		if isDir && r.matchPath(rel, base) {
			return true
		}
		// In gitignore, "build/" also ignores files and subdirectories nested under "build/...".
		if strings.Contains(rel, "/") {
			parts := strings.Split(rel, "/")
			for i := 0; i < len(parts)-1; i++ {
				sub := strings.Join(parts[:i+1], "/")
				if r.matchPath(sub, parts[i]) {
					return true
				}
			}
		}
		return false
	}
	if r.matchPath(rel, base) {
		return true
	}
	// If an un-negated rule matched an ancestor directory segment, files and subdirectories underneath are ignored too.
	if !r.negated && strings.Contains(rel, "/") {
		parts := strings.Split(rel, "/")
		for i := 0; i < len(parts)-1; i++ {
			sub := strings.Join(parts[:i+1], "/")
			if r.matchPath(sub, parts[i]) {
				return true
			}
		}
	}
	return false
}

func (r ignoreRule) matchPath(rel, base string) bool {
	if r.anchored || r.hasSlash {
		return matchGlob(r.pattern, rel)
	}
	if matchGlob(r.pattern, base) {
		return true
	}
	return matchGlob(r.pattern, rel)
}

// matchGlob matches pattern against target supporting '*', '?', and '**' path segments.
func matchGlob(pattern, target string) bool {
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, target)
		return err == nil && ok
	}
	patParts := strings.Split(pattern, "/")
	tgtParts := strings.Split(target, "/")
	return matchSegments(patParts, tgtParts)
}

func matchSegments(pat, tgt []string) bool {
	if len(pat) == 0 {
		return len(tgt) == 0
	}
	if pat[0] == "**" {
		// Collapse consecutive "**" segments.
		next := 1
		for next < len(pat) && pat[next] == "**" {
			next++
		}
		if next == len(pat) {
			return len(tgt) > 0
		}
		for i := 0; i <= len(tgt); i++ {
			if matchSegments(pat[next:], tgt[i:]) {
				return true
			}
		}
		return false
	}
	if len(tgt) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], tgt[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], tgt[1:])
}
