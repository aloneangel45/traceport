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
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		rule := ignoreRule{}
		pat := trimmed
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

		if strings.HasSuffix(pat, "/") {
			rule.dirOnly = true
			pat = strings.TrimSuffix(pat, "/")
			if pat == "" {
				continue
			}
		}

		if strings.HasPrefix(pat, "/") {
			rule.anchored = true
			pat = strings.TrimPrefix(pat, "/")
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

// ShouldIgnore reports whether rel (slash-separated path relative to workspace root)
// should be excluded. isDir indicates whether rel is a directory.
func (m *IgnoreMatcher) ShouldIgnore(rel string, isDir bool) bool {
	if m == nil {
		return false
	}
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
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
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	if rel == "" || rel == "." {
		return false
	}
	parts := strings.Split(rel, "/")
	if m.includeDefaultDirs && defaultWatchIgnoredDirs[parts[len(parts)-1]] {
		return true
	}
	if !m.ShouldIgnore(rel, true) {
		return false
	}
	// If any negation rule exists, only skip if no negation rule targets inside this directory.
	if m.hasNegation {
		prefix := rel + "/"
		for _, r := range m.rules {
			if !r.negated {
				continue
			}
			if !r.hasSlash || strings.HasPrefix(r.pattern, prefix) || strings.HasPrefix(r.pattern, "**/") {
				return false
			}
		}
	}
	return true
}

func (r ignoreRule) matches(rel, base string, isDir bool) bool {
	if r.dirOnly && !isDir {
		// In gitignore, "build/" also ignores files nested under "build/...".
		parts := strings.Split(rel, "/")
		for i := 0; i < len(parts)-1; i++ {
			sub := strings.Join(parts[:i+1], "/")
			if r.matchPath(sub, parts[i]) {
				return true
			}
		}
		return false
	}
	if r.matchPath(rel, base) {
		return true
	}
	// If an un-negated directory rule matched an ancestor segment, files underneath are ignored too.
	if !r.negated && !isDir && strings.Contains(rel, "/") {
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
			return true
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
