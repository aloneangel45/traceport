package checkpoint

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// ZstdMagic is the 4-byte little-endian magic header defined in RFC 8878 (0xFD2FB528).
var ZstdMagic = [4]byte{0x28, 0xB5, 0x2F, 0xFD}

// DefaultMaxDecodeMemory bounds single-frame Zstandard decompression memory (256 MiB)
// to protect against malformed or adversarial decompression bombs.
const DefaultMaxDecodeMemory = 256 << 20

// IsZstdFrame reports whether data begins with the RFC 8878 Zstandard frame magic bytes.
func IsZstdFrame(data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], ZstdMagic[:])
}

// CompressorConfig configures a reusable Zstandard Compressor.
type CompressorConfig struct {
	Level        zstd.EncoderLevel
	EncoderDict  []byte
	DecoderDicts [][]byte
	MaxDecodeMem uint64
}

// CompressorOption mutates CompressorConfig.
type CompressorOption func(*CompressorConfig)

// WithLevel sets the Zstandard encoder compression level.
func WithLevel(level zstd.EncoderLevel) CompressorOption {
	return func(cfg *CompressorConfig) {
		cfg.Level = level
	}
}

// WithEncoderDictionary sets the active dictionary used when encoding blobs.
// The dictionary is also automatically registered with the decoder.
func WithEncoderDictionary(dictBytes []byte) CompressorOption {
	return func(cfg *CompressorConfig) {
		if len(dictBytes) > 0 {
			cp := make([]byte, len(dictBytes))
			copy(cp, dictBytes)
			cfg.EncoderDict = cp
		}
	}
}

// WithDecoderDictionaries registers additional dictionaries for decoding historical blobs.
func WithDecoderDictionaries(dicts ...[]byte) CompressorOption {
	return func(cfg *CompressorConfig) {
		for _, d := range dicts {
			if len(d) > 0 {
				cp := make([]byte, len(d))
				copy(cp, d)
				cfg.DecoderDicts = append(cfg.DecoderDicts, cp)
			}
		}
	}
}

// WithMaxDecodeMemory overrides the maximum allowed decoder window/frame allocation.
func WithMaxDecodeMemory(limitBytes uint64) CompressorOption {
	return func(cfg *CompressorConfig) {
		if limitBytes > 0 {
			cfg.MaxDecodeMem = limitBytes
		}
	}
}

// Compressor provides goroutine-safe stateless Zstandard compression and decompression
// with optional dictionary acceleration and transparent raw-blob fallback.
type Compressor struct {
	mu           sync.RWMutex
	level        zstd.EncoderLevel
	maxDecodeMem uint64
	activeDict   []byte
	activeDictID uint32
	decoderDicts [][]byte
	enc          *zstd.Encoder
	dec          *zstd.Decoder
}

// NewCompressor constructs a reusable goroutine-safe Zstandard Compressor.
// By default it uses zstd.SpeedFastest and registers DefaultSourceDict with the decoder.
func NewCompressor(opts ...CompressorOption) (*Compressor, error) {
	cfg := CompressorConfig{
		Level:        zstd.SpeedFastest,
		MaxDecodeMem: DefaultMaxDecodeMemory,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	c := &Compressor{
		level:        cfg.Level,
		maxDecodeMem: cfg.MaxDecodeMem,
	}

	// Always register the deterministic built-in source dictionary for decoding.
	if builtIn := DefaultSourceDict(); len(builtIn) > 0 {
		c.decoderDicts = append(c.decoderDicts, builtIn)
	}
	for _, d := range cfg.DecoderDicts {
		c.addDecoderDictLocked(d)
	}
	if len(cfg.EncoderDict) > 0 {
		info, err := zstd.InspectDictionary(cfg.EncoderDict)
		if err != nil {
			return nil, fmt.Errorf("invalid zstd encoder dictionary: %w", err)
		}
		c.activeDict = cfg.EncoderDict
		c.activeDictID = info.ID()
		c.addDecoderDictLocked(cfg.EncoderDict)
	}

	if err := c.rebuildCodecsLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Compressor) addDecoderDictLocked(d []byte) {
	if len(d) == 0 {
		return
	}
	for _, existing := range c.decoderDicts {
		if bytes.Equal(existing, d) {
			return
		}
	}
	cp := make([]byte, len(d))
	copy(cp, d)
	c.decoderDicts = append(c.decoderDicts, cp)
}

func (c *Compressor) rebuildCodecsLocked() error {
	eOpts := []zstd.EOption{
		zstd.WithEncoderLevel(c.level),
		zstd.WithEncoderCRC(true),
		zstd.WithZeroFrames(true),
		zstd.WithEncoderConcurrency(1),
	}
	if len(c.activeDict) > 0 {
		eOpts = append(eOpts, zstd.WithEncoderDict(c.activeDict))
	}

	enc, err := zstd.NewWriter(nil, eOpts...)
	if err != nil {
		return fmt.Errorf("failed to initialize zstd encoder: %w", err)
	}

	dOpts := []zstd.DOption{
		zstd.WithDecoderConcurrency(0),
		zstd.WithDecoderMaxMemory(c.maxDecodeMem),
	}
	if len(c.decoderDicts) > 0 {
		dOpts = append(dOpts, zstd.WithDecoderDicts(c.decoderDicts...))
	}

	dec, err := zstd.NewReader(nil, dOpts...)
	if err != nil {
		_ = enc.Close()
		return fmt.Errorf("failed to initialize zstd decoder: %w", err)
	}

	oldEnc := c.enc
	oldDec := c.dec
	c.enc = enc
	c.dec = dec

	if oldEnc != nil {
		_ = oldEnc.Close()
	}
	if oldDec != nil {
		oldDec.Close()
	}
	return nil
}

// Compress encodes src into a complete Zstandard frame (including empty slices via ZeroFrames).
func (c *Compressor) Compress(src []byte) []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()

	dstCap := len(src)/2 + 32
	if dstCap < 32 {
		dstCap = 32
	}
	return c.enc.EncodeAll(src, make([]byte, 0, dstCap))
}

// Decompress decodes a Zstandard frame strictly, returning an error if src is not valid Zstd.
func (c *Compressor) Decompress(src []byte) ([]byte, error) {
	if !IsZstdFrame(src) {
		return nil, fmt.Errorf("missing zstd RFC 8878 magic header")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	out, err := c.dec.DecodeAll(src, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decode failed: %w", err)
	}
	if out == nil {
		return []byte{}, nil
	}
	return out, nil
}

// DecompressAuto inspects the first 4 bytes for RFC 8878 magic bytes (0x28 0xB5 0x2F 0xFD).
// Zstandard frames are decoded via DecodeAll; legacy/uncompressed payloads are returned as-is.
func (c *Compressor) DecompressAuto(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return []byte{}, nil
	}
	if IsZstdFrame(src) {
		return c.Decompress(src)
	}
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

// SetEncoderDict updates the active dictionary used for encoding and registers it for decoding.
// Passing nil or an empty slice disables dictionary encoding while retaining decoder dictionaries.
func (c *Compressor) SetEncoderDict(dictBytes []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(dictBytes) == 0 {
		c.activeDict = nil
		c.activeDictID = 0
		return c.rebuildCodecsLocked()
	}

	info, err := zstd.InspectDictionary(dictBytes)
	if err != nil {
		return fmt.Errorf("invalid zstd dictionary: %w", err)
	}

	cp := make([]byte, len(dictBytes))
	copy(cp, dictBytes)
	c.activeDict = cp
	c.activeDictID = info.ID()
	c.addDecoderDictLocked(cp)
	return c.rebuildCodecsLocked()
}

// RegisterDecoderDicts adds one or more dictionaries to the decoder without changing the encoder.
func (c *Compressor) RegisterDecoderDicts(dicts ...[]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, d := range dicts {
		if len(d) == 0 {
			continue
		}
		if _, err := zstd.InspectDictionary(d); err != nil {
			return fmt.Errorf("invalid zstd decoder dictionary: %w", err)
		}
		c.addDecoderDictLocked(d)
	}
	return c.rebuildCodecsLocked()
}

// DictID returns the active encoder dictionary ID, or 0 if raw Zstd encoding is active.
func (c *Compressor) DictID() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activeDictID
}

// Close releases resources held by the underlying Zstd encoder and decoder.
func (c *Compressor) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var err error
	if c.enc != nil {
		err = c.enc.Close()
		c.enc = nil
	}
	if c.dec != nil {
		c.dec.Close()
		c.dec = nil
	}
	return err
}
