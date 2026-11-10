package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func populateBenchmarkWorkspace(tb testing.TB, dir string) int64 {
	tb.Helper()
	var totalBytes int64

	write := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			tb.Fatal(err)
		}
		b := []byte(content)
		if err := os.WriteFile(full, b, 0644); err != nil {
			tb.Fatal(err)
		}
		totalBytes += int64(len(b))
	}

	for i := 0; i < 12; i++ {
		write(fmt.Sprintf("pkg/service/handler_%02d.go", i), fmt.Sprintf(`package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Request%d struct {
	SessionID string            `+"`json:\"session_id\"`"+`
	Step      int               `+"`json:\"step\"`"+`
	Metadata  map[string]string `+"`json:\"metadata,omitempty\"`"+`
}

func HandleEndpoint%d(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return errors.New("method not allowed")
	}
	var req Request%d
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("decode request %d: %%w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, "{\"status\":\"ok\",\"ts\":%%d}\n", time.Now().UnixNano())
	return nil
}
`, i, i, i, i))
	}

	for i := 0; i < 8; i++ {
		write(fmt.Sprintf("agent/tools/tool_%02d.py", i), fmt.Sprintf(`from __future__ import annotations
import dataclasses
import json
from typing import Any, Dict

@dataclasses.dataclass(frozen=True)
class ToolSpec%d:
    name: str = "workspace_editor_%d"
    timeout_sec: float = 30.0

def execute_tool_%d(payload: Dict[str, Any]) -> str:
    if "path" not in payload:
        raise ValueError("missing required parameter 'path'")
    return json.dumps({"ok": True, "tool": %d, "path": payload["path"]})
`, i, i, i, i))
	}

	for i := 0; i < 8; i++ {
		write(fmt.Sprintf("web/src/components/View%02d.ts", i), fmt.Sprintf(`export interface FrameProps%d {
  readonly index: number;
  readonly status: "running" | "completed" | "aborted";
  readonly snapshotHash: string;
}

export function formatFrameSummary%d(props: FrameProps%d): string {
  return "[step " + String(props.index) + "] " + props.status + " (" + props.snapshotHash.slice(0, 12) + ")";
}
`, i, i, i))
	}

	for i := 0; i < 4; i++ {
		write(fmt.Sprintf("configs/pipeline_%02d.yaml", i), fmt.Sprintf(`version: "1.1"
pipeline: traceport_stage_%d
runner:
  cgo_enabled: false
  compression: zstd
  max_dict_bytes: 32768
steps:
  - name: verify
    command: traceport verify --json
  - name: test
    command: go test -v ./...
`, i))
	}

	return totalBytes
}

func directoryDiskBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := d.Name()
		if base == "stat_cache.json" || base == "active" || filepath.Ext(base) == ".zdict" {
			return nil
		}
		if info, iErr := d.Info(); iErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// pureGoWALStore models a 4096-byte paged in-memory SQLite WAL database storing full
// snapshot file rows inside 24-byte framed WAL pages with IEEE CRC32 frame checksums.
type pureGoWALStore struct {
	walBuf    bytes.Buffer
	pageSeq   uint32
	snapshots map[int]map[string][]byte
	dedupRows map[string]uint32 // sha256 -> first WAL page offset
}

const (
	sqliteWALHeaderBytes      = 32
	sqliteWALFrameHeaderBytes = 24
	sqliteDefaultPageBytes    = 4096
)

func newPureGoWALStore() *pureGoWALStore {
	s := &pureGoWALStore{
		snapshots: make(map[int]map[string][]byte),
		dedupRows: make(map[string]uint32),
	}
	// Write 32-byte WAL file header.
	var hdr [sqliteWALHeaderBytes]byte
	binary.BigEndian.PutUint32(hdr[0:4], 0x377f0682)
	binary.BigEndian.PutUint32(hdr[4:8], 3007000)
	binary.BigEndian.PutUint32(hdr[8:12], sqliteDefaultPageBytes)
	s.walBuf.Write(hdr[:])
	return s
}

func (w *pureGoWALStore) appendPages(payload []byte) {
	rem := payload
	for len(rem) > 0 || len(payload) == 0 {
		w.pageSeq++
		var frameHdr [sqliteWALFrameHeaderBytes]byte
		binary.BigEndian.PutUint32(frameHdr[0:4], w.pageSeq)
		chunk := rem
		if len(chunk) > sqliteDefaultPageBytes {
			chunk = rem[:sqliteDefaultPageBytes]
		}
		var page [sqliteDefaultPageBytes]byte
		copy(page[:], chunk)
		crc := crc32.ChecksumIEEE(page[:])
		binary.BigEndian.PutUint32(frameHdr[16:20], crc)
		w.walBuf.Write(frameHdr[:])
		w.walBuf.Write(page[:])
		if len(rem) <= sqliteDefaultPageBytes {
			break
		}
		rem = rem[sqliteDefaultPageBytes:]
	}
}

func (w *pureGoWALStore) createSnapshot(step int, workDir string) error {
	snap := make(map[string][]byte)
	var manifestRows []FileEntry

	err := filepath.WalkDir(workDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || path == workDir {
			return nil
		}
		rel, err := filepath.Rel(workDir, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		if _, seen := w.dedupRows[sha]; !seen {
			w.dedupRows[sha] = w.pageSeq + 1
			w.appendPages(data)
		}
		snap[relSlash] = data
		manifestRows = append(manifestRows, FileEntry{
			Path:   relSlash,
			SHA256: sha,
			Size:   int64(len(data)),
			Mode:   0644,
		})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(manifestRows, func(i, j int) bool { return manifestRows[i].Path < manifestRows[j].Path })
	metaBytes, _ := json.Marshal(manifestRows)
	w.appendPages(metaBytes)
	w.snapshots[step] = snap
	return nil
}

func (w *pureGoWALStore) restoreSnapshot(step int, workDir string) error {
	snap, ok := w.snapshots[step]
	if !ok {
		return fmt.Errorf("missing WAL snapshot %d", step)
	}
	_ = filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == workDir {
			return nil
		}
		rel, rErr := filepath.Rel(workDir, path)
		if rErr == nil {
			if _, exists := snap[filepath.ToSlash(rel)]; !exists {
				_ = os.Remove(path)
			}
		}
		return nil
	})
	for rel, content := range snap {
		full := filepath.Join(workDir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(full), 0755)
		if err := os.WriteFile(full, content, 0644); err != nil {
			return err
		}
	}
	return nil
}

func BenchmarkShadowTree(b *testing.B) {
	modes := []struct {
		name string
		mode TreeCompressionMode
		wal  bool
	}{
		{name: "Uncompressed", mode: ModeUncompressed},
		{name: "ZstdRaw", mode: ModeZstdRaw},
		{name: "ZstdDict", mode: ModeZstdDict},
		{name: "PureGoSQLiteWAL", wal: true},
	}

	for _, bm := range modes {
		b.Run(bm.name, func(b *testing.B) {
			workDir := b.TempDir()
			baseBytes := populateBenchmarkWorkspace(b, workDir)

			// Pre-train a workspace dictionary once for ZstdDict mode.
			dictStoreDir := b.TempDir()
			var trainedDict []byte
			if bm.mode == ModeZstdDict && !bm.wal {
				if _, err := TrainDictionaryFromDir(workDir, dictStoreDir); err != nil {
					b.Fatal(err)
				}
				var err error
				trainedDict, _, err = LoadStoreDictionaries(dictStoreDir)
				if err != nil {
					b.Fatal(err)
				}
			}

			var totalStoredBytes int64
			var totalLogicalBytes int64
			var totalRollbackUS float64

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				iterStore := filepath.Join(b.TempDir(), fmt.Sprintf("store_%d", i))
				_ = os.MkdirAll(iterStore, 0700)
				// Reset mutated files before each 5-step agent trajectory.
				targetGo := filepath.Join(workDir, "pkg", "service", "handler_00.go")
				origGo, _ := os.ReadFile(targetGo)
				extraTemp := filepath.Join(workDir, "agent_scratch.json")
				_ = os.Remove(extraTemp)
				var ts *TreeStore
				if !bm.wal {
					opts := []TreeOption{WithCompressionMode(bm.mode)}
					if len(trainedDict) > 0 {
						opts = append(opts, WithCustomDictionary(trainedDict))
					}
					var err error
					ts, err = NewTreeStore(workDir, iterStore, opts...)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()

				if bm.wal {
					wal := newPureGoWALStore()
					// Step 1: initial full snapshot
					if err := wal.createSnapshot(1, workDir); err != nil {
						b.Fatal(err)
					}
					// Steps 2..5: incremental agent mutations
					for step := 2; step <= 5; step++ {
						mut := append([]byte{}, origGo...)
						mut = append(mut, []byte(fmt.Sprintf("\n// agent edit step %d iter %d\n", step, i))...)
						_ = os.WriteFile(targetGo, mut, 0644)
						if step == 4 {
							scratch := []byte(fmt.Sprintf(`{"scratch_step":%d,"iter":%d}`, step, i))
							_ = os.WriteFile(extraTemp, scratch, 0644)
						}
						if err := wal.createSnapshot(step, workDir); err != nil {
							b.Fatal(err)
						}
					}
					rbStart := time.Now()
					if err := wal.restoreSnapshot(2, workDir); err != nil {
						b.Fatal(err)
					}
					totalRollbackUS += float64(time.Since(rbStart).Nanoseconds()) / 1000.0
					b.StopTimer()
					totalStoredBytes += int64(wal.walBuf.Len())
					totalLogicalBytes += 5 * baseBytes
				} else {
					var step2Hash string
					if _, err := ts.CreateSnapshot(); err != nil {
						b.Fatal(err)
					}
					for step := 2; step <= 5; step++ {
						mut := append([]byte{}, origGo...)
						mut = append(mut, []byte(fmt.Sprintf("\n// agent edit step %d iter %d\n", step, i))...)
						_ = os.WriteFile(targetGo, mut, 0644)
						if step == 4 {
							scratch := []byte(fmt.Sprintf(`{"scratch_step":%d,"iter":%d}`, step, i))
							_ = os.WriteFile(extraTemp, scratch, 0644)
						}
						h, err := ts.CreateSnapshot()
						if err != nil {
							b.Fatal(err)
						}
						if step == 2 {
							step2Hash = h
						}
					}

					rbStart := time.Now()
					if err := ts.RestoreSnapshot(step2Hash); err != nil {
						b.Fatal(err)
					}
					totalRollbackUS += float64(time.Since(rbStart).Nanoseconds()) / 1000.0
					b.StopTimer()
					_ = ts.Close()

					totalStoredBytes += directoryDiskBytes(iterStore)
					totalLogicalBytes += 5 * baseBytes
				}

				_ = os.WriteFile(targetGo, origGo, 0644)
				_ = os.Remove(extraTemp)
				b.StartTimer()
			}

			avgStored := float64(totalStoredBytes) / float64(b.N)
			ratioPct := 0.0
			if totalLogicalBytes > 0 {
				ratioPct = float64(totalStoredBytes) * 100.0 / float64(totalLogicalBytes)
			}
			avgRollbackUS := totalRollbackUS / float64(b.N)

			b.ReportMetric(ratioPct, "ratio_pct")
			b.ReportMetric(avgStored, "stored_bytes/op")
			b.ReportMetric(avgRollbackUS, "rollback_us/op")
		})
	}
}

func BenchmarkCompressorSmallSource(b *testing.B) {
	sample := []byte(`package recorder

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

func ReadChunked(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("stream read error: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), err
}
`)

	b.Run("ZstdRaw", func(b *testing.B) {
		c, err := NewCompressor()
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		b.SetBytes(int64(len(sample)))
		b.ResetTimer()
		var compLen int
		for i := 0; i < b.N; i++ {
			enc := c.Compress(sample)
			compLen = len(enc)
		}
		b.ReportMetric(float64(compLen)*100.0/float64(len(sample)), "ratio_pct")
	})

	b.Run("ZstdDict", func(b *testing.B) {
		c, err := NewCompressor(WithEncoderDictionary(DefaultSourceDict()))
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		b.SetBytes(int64(len(sample)))
		b.ResetTimer()
		var compLen int
		for i := 0; i < b.N; i++ {
			enc := c.Compress(sample)
			compLen = len(enc)
		}
		b.ReportMetric(float64(compLen)*100.0/float64(len(sample)), "ratio_pct")
	})
}

func BenchmarkIncrementalStatCache_Unchanged(b *testing.B) {
	workDir := b.TempDir()
	storeDir := b.TempDir()
	populateBenchmarkWorkspace(b, workDir)

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		b.Fatal(err)
	}
	defer ts.Close()

	if _, err := ts.CreateSnapshot(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ts.CreateSnapshot(); err != nil {
			b.Fatal(err)
		}
	}
}
