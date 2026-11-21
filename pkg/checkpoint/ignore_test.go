package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreMatcher_GitignoreEdgeCasesAndWindowsSlashes(t *testing.T) {
	root := t.TempDir()

	// Include UTF-8 BOM on line 1, comments, indented comments, escaped \# and \!,
	// leading/trailing spaces, escaped trailing space, dir-only trailing '/',
	// negation rules (including glob negations), trailing /**, and Windows backslash rules.
	gitignore := "\uFEFF# top comment\r\n" +
		"   # indented comment\r\n" +
		"  *.log   \r\n" +
		"!important.log\r\n" +
		"\\#literal-hash.txt\r\n" +
		"\\!literal-bang.txt\r\n" +
		"trailing-space\\ \r\n" +
		"build/\r\n" +
		"!build/keep.txt\r\n" +
		"globdir/\r\n" +
		"!*/must-keep.txt\r\n" +
		"deep/sub/\r\n" +
		"!deep/*/anchor.txt\r\n" +
		"cache/**\r\n" +
		"winpkg\\out\\\r\n"

	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(gitignore), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".traceportignore"), []byte("  scratch.tmp  \n/root-only.dat\n"), 0644); err != nil {
		t.Fatal(err)
	}

	m := LoadIgnoreMatcher(root, true)

	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		// Comments are not matched as filenames, escaped \# and \! ARE matched.
		{"# top comment", false, false},
		{"#literal-hash.txt", false, true},
		{"!literal-bang.txt", false, true},

		// Trimmed spaces vs escaped trailing space.
		{"debug.log", false, true},
		{"sub/debug.log", false, true},
		{"important.log", false, false},
		{"sub/important.log", false, false},
		{"trailing-space ", false, true},
		{"trailing-space", false, false},
		{"scratch.tmp", false, true},

		// Anchored leading '/' rule.
		{"root-only.dat", false, true},
		{"nested/root-only.dat", false, false},

		// Dir-only trailing '/': ignores dir, nested files, AND nested subdirs, but NOT regular file named "build".
		{"build", false, false},
		{"build", true, true},
		{"build/", true, true},
		{"build/app.o", false, true},
		{"build/sub", true, true},
		{"build/sub/x.o", false, true},
		{"build/keep.txt", false, false},

		// Windows backslashes in rel path must match on ALL OSes (Linux/macOS/Windows)!
		{`build\app.o`, false, true},
		{`build\sub\x.o`, false, true},
		{`build\keep.txt`, false, false},
		{`winpkg\out\bundle.js`, false, true},
		{`winpkg/out/bundle.js`, false, true},
		{`node_modules\pkg\index.js`, false, true},

		// Glob negation under ignored dirs.
		{"globdir/drop.txt", false, true},
		{"globdir/must-keep.txt", false, false},
		{"deep/sub/drop.txt", false, true},
		{"deep/sub/anchor.txt", false, false},

		// Trailing /** matches descendants only, not the dir/file itself.
		{"cache", false, false},
		{"cache", true, false},
		{"cache/a.bin", false, true},
		{"cache/nested/b.bin", false, true},
	}

	for _, tc := range cases {
		if got := m.ShouldIgnore(tc.rel, tc.isDir); got != tc.want {
			t.Errorf("ShouldIgnore(%q, isDir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}

	// Verify CanSkipDir respects negation rules (including glob negations!) while still pruning safe subdirs.
	skipCases := []struct {
		dir  string
		want bool
	}{
		{"node_modules", true},
		{`node_modules\sub`, true},
		{"build", false},        // negated by !build/keep.txt (and unanchored !important.log)
		{"build/sub", false},    // unanchored !important.log could match build/sub/important.log
		{"globdir", false},      // negated by !*/must-keep.txt
		{"deep/sub", false},     // negated by !deep/*/anchor.txt
		{"winpkg/out", false},   // negated by unanchored !important.log (could match inside)
	}
	for _, tc := range skipCases {
		if got := m.CanSkipDir(tc.dir); got != tc.want {
			t.Errorf("CanSkipDir(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}

	// Matcher with ONLY path-scoped negation must safely skip unrelated ignored dirs.
	root2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(root2, ".gitignore"), []byte("dist/\nother/\n!dist/keep.txt\n"), 0644)
	m2 := LoadIgnoreMatcher(root2, true)
	if m2.CanSkipDir("dist") {
		t.Errorf("expected CanSkipDir(dist) == false due to !dist/keep.txt")
	}
	if !m2.CanSkipDir("other") {
		t.Errorf("expected CanSkipDir(other) == true since !dist/keep.txt cannot match inside other/")
	}
	if !m2.CanSkipDir("dist/nested") {
		t.Errorf("expected CanSkipDir(dist/nested) == true since !dist/keep.txt is not inside dist/nested/")
	}
}
