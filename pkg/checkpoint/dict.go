package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"
)

const (
	// DefaultSourceDictID is the deterministic Zstandard dictionary ID ("TP01")
	// for the built-in canonical multi-language source dictionary.
	DefaultSourceDictID uint32 = 0x54503031

	// MaxDictSampleBytes caps individual file samples during workspace dictionary training (64 KiB).
	MaxDictSampleBytes = 64 * 1024

	// MaxDictTotalSamples caps the number of workspace files sampled during training.
	MaxDictTotalSamples = 2048

	// DefaultMaxDictBytes sets the maximum trained Zstandard dictionary size (32 KiB).
	DefaultMaxDictBytes = 32 * 1024
)

var (
	defaultDictOnce sync.Once
	defaultDictData []byte
)

var skippedDictDirs = map[string]bool{
	".git":         true,
	".traceport":   true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"target":       true,
	".next":        true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	"blobs":        true,
	"trees":        true,
	"dicts":        true,
}

var binaryExtensions = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".a": true, ".o": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".ico": true,
	".pdf": true, ".zip": true, ".tar": true, ".gz": true, ".zst": true, ".zdict": true,
	".wasm": true, ".pyc": true, ".class": true, ".jar": true, ".bin": true, ".db": true,
	".sqlite": true, ".mp3": true, ".mp4": true, ".woff": true, ".woff2": true, ".ttf": true,
}

// DictStats reports metrics from training or inspecting a workspace Zstandard dictionary.
type DictStats struct {
	DictID              uint32        `json:"dict_id"`
	DictPath            string        `json:"dict_path"`
	DictBytes           int           `json:"dict_bytes"`
	SampleCount         int           `json:"sample_count"`
	RawSampleBytes      int64         `json:"raw_sample_bytes"`
	RawCompressedBytes  int64         `json:"raw_compressed_bytes"`
	DictCompressedBytes int64         `json:"dict_compressed_bytes"`
	RatioRawPct         float64       `json:"ratio_raw_pct"`
	RatioDictPct        float64       `json:"ratio_dict_pct"`
	SavingsGainPct      float64       `json:"savings_gain_pct"`
	Duration            time.Duration `json:"duration_ns"`
}

// DefaultSourceDict returns the deterministic built-in Zstandard dictionary trained over
// canonical Go, Python, TypeScript/JS, Rust, C, JSON, YAML, Markdown, Shell, and SQL constructs.
func DefaultSourceDict() []byte {
	defaultDictOnce.Do(func() {
		samples := canonicalSourceCorpus()
		built, err := dict.BuildZstdDict(samples, dict.Options{
			MaxDictSize: DefaultMaxDictBytes,
			HashBytes:   6,
			ZstdDictID:  DefaultSourceDictID,
			ZstdLevel:   zstd.SpeedFastest,
		})
		if err == nil && len(built) > 0 {
			defaultDictData = built
		}
	})
	if len(defaultDictData) == 0 {
		return nil
	}
	cp := make([]byte, len(defaultDictData))
	copy(cp, defaultDictData)
	return cp
}

// TrainDictionaryFromDir scans source files in workspaceDir, trains a deterministic Zstd
// dictionary, persists it under <storeDir>/dicts/, marks it active, and returns compression metrics.
func TrainDictionaryFromDir(workspaceDir, storeDir string) (*DictStats, error) {
	start := time.Now()

	absWork, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	absStore, err := filepath.Abs(storeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve store path: %w", err)
	}

	samples, rawBytes, err := collectWorkspaceSamples(absWork, absStore)
	if err != nil {
		return nil, err
	}

	sampleCount := len(samples)

	// Build training input; if workspace has few small files, supplement with canonical
	// multi-language constructs while keeping workspace files weighted first.
	trainInput := make([][]byte, 0, len(samples)+32)
	var trainTotal int
	for _, s := range samples {
		trainInput = append(trainInput, s)
		trainTotal += len(s)
	}
	if len(trainInput) < 16 || trainTotal < 16*1024 {
		// Weight existing workspace files higher before adding canonical baseline samples.
		for _, s := range samples {
			trainInput = append(trainInput, s)
			trainTotal += len(s)
		}
		for _, c := range canonicalSourceCorpus() {
			trainInput = append(trainInput, c)
			trainTotal += len(c)
		}
	}

	dictID := deriveDeterministicDictID(trainInput)
	dictBytes, err := dict.BuildZstdDict(trainInput, dict.Options{
		MaxDictSize: DefaultMaxDictBytes,
		HashBytes:   6,
		ZstdDictID:  dictID,
		ZstdLevel:   zstd.SpeedFastest,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build zstd dictionary: %w", err)
	}

	dictsDir := filepath.Join(absStore, "dicts")
	if err := os.MkdirAll(dictsDir, 0700); err != nil {
		return nil, fmt.Errorf("create dicts directory: %w", err)
	}

	fileName := fmt.Sprintf("dict_%08x.zdict", dictID)
	dictPath := filepath.Join(dictsDir, fileName)
	if err := writeAtomicFile(dictPath, dictBytes, 0600); err != nil {
		return nil, fmt.Errorf("persist dictionary: %w", err)
	}
	activePath := filepath.Join(dictsDir, "active")
	if err := writeAtomicFile(activePath, []byte(fileName+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("persist active dictionary pointer: %w", err)
	}

	// Measure compression ratio on workspace samples (or training corpus if workspace was empty).
	evalSamples := samples
	evalRawBytes := rawBytes
	if len(evalSamples) == 0 {
		evalSamples = canonicalSourceCorpus()
		for _, s := range evalSamples {
			evalRawBytes += int64(len(s))
		}
	}

	rawComp, err := NewCompressor(WithLevel(zstd.SpeedFastest))
	if err != nil {
		return nil, err
	}
	defer rawComp.Close()

	dictComp, err := NewCompressor(WithLevel(zstd.SpeedFastest), WithEncoderDictionary(dictBytes))
	if err != nil {
		return nil, err
	}
	defer dictComp.Close()

	var rawCompressed, dictCompressed int64
	for _, s := range evalSamples {
		rawCompressed += int64(len(rawComp.Compress(s)))
		dictCompressed += int64(len(dictComp.Compress(s)))
	}

	var ratioRaw, ratioDict, gain float64
	if evalRawBytes > 0 {
		ratioRaw = float64(rawCompressed) * 100.0 / float64(evalRawBytes)
		ratioDict = float64(dictCompressed) * 100.0 / float64(evalRawBytes)
		gain = ratioRaw - ratioDict
	}

	return &DictStats{
		DictID:              dictID,
		DictPath:            dictPath,
		DictBytes:           len(dictBytes),
		SampleCount:         sampleCount,
		RawSampleBytes:      evalRawBytes,
		RawCompressedBytes:  rawCompressed,
		DictCompressedBytes: dictCompressed,
		RatioRawPct:         ratioRaw,
		RatioDictPct:        ratioDict,
		SavingsGainPct:      gain,
		Duration:            time.Since(start),
	}, nil
}

// LoadStoreDictionaries loads all persisted .zdict dictionaries in <storeDir>/dicts/
// and returns (activeEncoderDict, allDecoderDicts). If no trained workspace dictionary exists,
// DefaultSourceDict() is returned as the active zero-cold-start dictionary.
func LoadStoreDictionaries(storeDir string) ([]byte, [][]byte, error) {
	dictsDir := filepath.Join(storeDir, "dicts")
	entries, err := os.ReadDir(dictsDir)
	if err != nil {
		if os.IsNotExist(err) {
			def := DefaultSourceDict()
			if len(def) > 0 {
				return def, [][]byte{def}, nil
			}
			return nil, nil, nil
		}
		return nil, nil, err
	}

	activeNameBytes, _ := os.ReadFile(filepath.Join(dictsDir, "active"))
	activeName := strings.TrimSpace(string(activeNameBytes))

	var allDicts [][]byte
	if def := DefaultSourceDict(); len(def) > 0 {
		allDicts = append(allDicts, def)
	}

	var activeDict []byte
	var latestDict []byte

	// Sort deterministically by filename.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".zdict") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dictsDir, name))
		if err != nil || len(data) == 0 {
			continue
		}
		if _, err := zstd.InspectDictionary(data); err != nil {
			continue
		}
		allDicts = append(allDicts, data)
		latestDict = data
		if name == activeName {
			activeDict = data
		}
	}

	if len(activeDict) == 0 {
		if len(latestDict) > 0 {
			activeDict = latestDict
		} else {
			activeDict = DefaultSourceDict()
		}
	}

	return activeDict, allDicts, nil
}

func collectWorkspaceSamples(absWork, absStore string) ([][]byte, int64, error) {
	var relPaths []string
	storeInsideWork := isSubpath(absWork, absStore) && !sameFilePath(absWork, absStore)
	sameWorkAndStore := sameFilePath(absWork, absStore)

	err := filepath.WalkDir(absWork, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == absWork {
			return nil
		}
		if storeInsideWork && (sameFilePath(path, absStore) || isSubpath(absStore, path)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if skippedDictDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}
		if sameWorkAndStore && isInternalStoreArtifact(name) {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if binaryExtensions[ext] {
			return nil
		}

		rel, err := filepath.Rel(absWork, path)
		if err != nil {
			return nil
		}
		relPaths = append(relPaths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	sort.Strings(relPaths)
	if len(relPaths) > MaxDictTotalSamples {
		relPaths = relPaths[:MaxDictTotalSamples]
	}

	samples := make([][]byte, 0, len(relPaths))
	var totalBytes int64

	for _, rel := range relPaths {
		full := filepath.Join(absWork, filepath.FromSlash(rel))
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		buf, err := io.ReadAll(io.LimitReader(f, MaxDictSampleBytes))
		_ = f.Close()
		if err != nil || len(buf) < 16 {
			continue
		}
		if isBinaryContent(buf) {
			continue
		}
		samples = append(samples, buf)
		totalBytes += int64(len(buf))
	}

	return samples, totalBytes, nil
}

func isBinaryContent(data []byte) bool {
	check := data
	if len(check) > 8192 {
		check = check[:8192]
	}
	if bytes.IndexByte(check, 0) >= 0 {
		return true
	}
	if !utf8.Valid(check) {
		// Allow trailing partial UTF-8 sequence at the 8 KiB / 64 KiB truncation boundary.
		trim := check
		for i := 0; i < 3 && len(trim) > 0; i++ {
			trim = trim[:len(trim)-1]
			if utf8.Valid(trim) {
				return false
			}
		}
		return true
	}
	return false
}

func deriveDeterministicDictID(samples [][]byte) uint32 {
	h := sha256.New()
	var lenBuf [8]byte
	for _, s := range samples {
		binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(s)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(s)
	}
	sum := h.Sum(nil)
	id := binary.LittleEndian.Uint32(sum[:4])
	// RFC 8878 reserves dictionary IDs < 32768 and >= (1<<31) for private/standard ranges;
	// ensure non-zero and distinct from DefaultSourceDictID.
	id = (id & 0x7FFFFFFF)
	if id < 32768 {
		id += 32768
	}
	if id == DefaultSourceDictID {
		id++
	}
	return id
}

func canonicalSourceCorpus() [][]byte {
	snippets := []string{
		// Go constructs
		`package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Service struct {
	mu     sync.RWMutex
	client *http.Client
	config map[string]string
}

func NewService(cfg map[string]string) (*Service, error) {
	if cfg == nil {
		return nil, errors.New("configuration must not be nil")
	}
	return &Service{
		client: &http.Client{Timeout: 30 * time.Second},
		config: cfg,
	}, nil
}
`,
		`package storage

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
)

func (s *Store) VerifyChecksum(data []byte, expected string) error {
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("checksum mismatch: got %s, want %s", actual, expected)
	}
	return nil
}

func TestVerifyChecksum(t *testing.T) {
	payload := []byte("deterministic snapshot payload")
	if len(payload) == 0 {
		t.Fatalf("unexpected empty payload")
	}
}
`,
		// Python constructs
		`from __future__ import annotations

import asyncio
import dataclasses
import json
import logging
import os
import pathlib
from typing import Any, Dict, Iterable, List, Optional, Tuple

logger = logging.getLogger(__name__)

@dataclasses.dataclass(frozen=True)
class ExecutionResult:
    step_index: int
    status: str
    output: str
    duration_ms: float
    metadata: Dict[str, Any] = dataclasses.field(default_factory=dict)

async def run_agent_step(prompt: str, timeout: float = 30.0) -> ExecutionResult:
    try:
        await asyncio.sleep(0.01)
        return ExecutionResult(step_index=1, status="completed", output=prompt, duration_ms=10.0)
    except Exception as exc:
        logger.exception("agent step failed: %s", exc)
        raise RuntimeError(f"execution error: {exc}") from exc
`,
		`import pytest
from pathlib import Path

def load_configuration(config_path: Path) -> dict[str, object]:
    if not config_path.exists():
        raise FileNotFoundError(f"missing configuration: {config_path}")
    with config_path.open("r", encoding="utf-8") as handle:
        data = json.load(handle)
    if not isinstance(data, dict):
        raise ValueError("expected dictionary root object")
    return data
`,
		// TypeScript / JavaScript constructs
		`import { readFile, writeFile, mkdir } from "node:fs/promises";
import { join, resolve, dirname } from "node:path";
import { createHash } from "node:crypto";

export interface SessionFrame {
  readonly index: number;
  readonly timestamp: string;
  readonly type: "input" | "output" | "tool_call" | "tool_result" | "error" | "checkpoint";
  readonly content: string;
  readonly gitSnapshot?: string;
}

export class ReplayController {
  private readonly frames: SessionFrame[] = [];

  public constructor(private readonly workspaceDir: string) {}

  public async appendFrame(frame: SessionFrame): Promise<void> {
    const payload = JSON.stringify(frame) + "\n";
    const digest = createHash("sha256").update(payload).digest("hex");
    if (digest.length === 64) {
      this.frames.push(frame);
    }
  }
}
`,
		`export async function fetchJson<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      ...(init?.headers ?? {}),
    },
  });
  if (!response.ok) {
    throw new Error("HTTP request failed with status " + String(response.status));
  }
  return (await response.json()) as T;
}
`,
		// Rust constructs
		`use std::collections::{BTreeMap, HashMap};
use std::fs::{self, File};
use std::io::{self, BufRead, BufReader, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, RwLock};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ManifestEntry {
    pub path: String,
    pub sha256: String,
    pub size: u64,
    pub mode: u32,
}

impl ManifestEntry {
    pub fn validate(&self) -> Result<(), String> {
        if self.path.is_empty() || self.path.contains("..") {
            return Err(format!("invalid relative path: {}", self.path));
        }
        if self.sha256.len() != 64 {
            return Err(String::from("invalid sha256 digest length"));
        }
        Ok(())
    }
}
`,
		// C / Systems constructs
		`#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

typedef struct {
    uint32_t magic;
    uint32_t version;
    uint64_t payload_len;
    uint8_t  digest[32];
} frame_header_t;

int validate_frame_header(const frame_header_t *hdr, size_t max_bytes) {
    if (hdr == NULL) {
        return -EINVAL;
    }
    if (hdr->magic != 0xFD2FB528U || hdr->payload_len > max_bytes) {
        return -EPROTO;
    }
    return 0;
}
`,
		// JSON / JSONL telemetry & manifest constructs
		`{
  "version": 1,
  "id": "trace_17939077467428723",
  "command": "go test -v ./...",
  "workspace_dir": "/workspace/project",
  "status": "completed",
  "exit_code": 0,
  "start_time": "2026-11-10T12:00:00Z",
  "end_time": "2026-11-10T12:00:05Z",
  "files": [
    {"path": "cmd/main.go", "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "size": 1024, "mode": 420},
    {"path": "pkg/server/handler.go", "sha256": "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3", "size": 4096, "mode": 420}
  ]
}
`,
		`{"index":1,"timestamp":"2026-11-10T12:00:01Z","type":"checkpoint","content":"Initial workspace snapshot","git_snapshot":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
{"index":2,"timestamp":"2026-11-10T12:00:02Z","type":"tool_call","content":"invoking edit_file","tool":{"name":"edit_file","arguments":{"path":"pkg/auth/middleware.go"}}}
{"index":3,"timestamp":"2026-11-10T12:00:03Z","type":"output","content":"=== RUN   TestMiddlewareAuth\n--- PASS: TestMiddlewareAuth (0.01s)"}
`,
		// YAML CI / configuration constructs
		`name: CI
on:
  push:
    branches: [ main ]
  pull_request:
    branches: [ main ]

jobs:
  test:
    runs-on: ${{ matrix.os }}
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, windows-latest, macos-latest]
        go-version: ['1.27.x', '1.26.x']
    steps:
      - name: Checkout repository
        uses: actions/checkout@v4
      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: ${{ matrix.go-version }}
      - name: Run tests
        env:
          CGO_ENABLED: 0
        run: go test -v ./...
`,
		// Markdown documentation constructs
		`# Architecture Overview

## Content-Addressed Shadow Trees

All workspace files are indexed into deterministic SHA-256 manifests and compressed with Zstandard.

### Usage

` + "```bash\ntraceport record -- go test ./...\ntraceport diff <session_id> --from 1 --to 3\ntraceport rewind <session_id> --step 2\n```" + `

| Metric | Raw | Zstd | Zstd + Dict |
| ------ | --- | ---- | ----------- |
| Ratio  | 100% | 34% | 19%         |
`,
		// Shell / POSIX script constructs
		`#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly BUILD_DIR="${SCRIPT_DIR}/dist"

log_info() {
    printf "[traceport] %s\n" "$1"
}

log_err() {
    printf "[err] %s\n" "$1" >&2
}

mkdir -p "${BUILD_DIR}"
if ! command -v sha256sum >/dev/null 2>&1; then
    shasum -a 256 "${BUILD_DIR}/checksums.txt"
fi
`,
		// SQL DDL & query constructs
		`CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY NOT NULL,
    command TEXT NOT NULL,
    workspace_dir TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'running',
    exit_code INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sessions_created_at ON sessions(created_at DESC);

SELECT s.id, s.command, s.status, COUNT(f.idx) AS frame_count
FROM sessions AS s
LEFT JOIN frames AS f ON f.session_id = s.id
WHERE s.status IN ('completed', 'aborted')
GROUP BY s.id, s.command, s.status
ORDER BY s.created_at DESC
LIMIT 50;
`,
	}

	// Expand into granular overlapping chunks + full snippets so dict.BuildZstdDict
	// indexes rich 6-byte n-grams across all 10 language families deterministically.
	out := make([][]byte, 0, len(snippets)*3)
	for _, s := range snippets {
		b := []byte(s)
		out = append(out, b)
		lines := bytes.Split(b, []byte("\n"))
		mid := len(lines) / 2
		if mid > 2 {
			firstHalf := bytes.Join(lines[:mid], []byte("\n"))
			secondHalf := bytes.Join(lines[mid:], []byte("\n"))
			if len(firstHalf) >= 32 {
				out = append(out, firstHalf)
			}
			if len(secondHalf) >= 32 {
				out = append(out, secondHalf)
			}
		}
	}
	return out
}
