package cmd

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// generateObjectNames creates n sorted object names with realistic paths.
func generateObjectNames(n int, pattern string) []string {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	names := make([]string, n)
	for i := 0; i < n; i++ {
		switch pattern {
		case "short":
			names[i] = fmt.Sprintf("obj-%08d.dat", i)
		case "medium":
			names[i] = fmt.Sprintf("data/2026/%02d/%02d/event-%08d.json", rng.Intn(12)+1, rng.Intn(28)+1, i)
		case "long":
			names[i] = fmt.Sprintf("us-east-1/production/events/2026/%02d/%02d/user-%06d/campaign-%04d/event-%08d.json",
				rng.Intn(12)+1, rng.Intn(28)+1, rng.Intn(100000), rng.Intn(1000), i)
		}
	}
	sort.Strings(names)
	return names
}

// compressTextChunk simulates what writeChunk does: join + zstd compress.
func compressTextChunk(names []string) ([]byte, error) {
	raw := []byte(strings.Join(names, "\n"))

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithWindowSize(1<<20))
	if err != nil {
		return nil, err
	}
	if _, err := enc.Write(raw); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decompressTextChunk simulates what readChunk does.
func decompressTextChunk(data []byte) ([]string, error) {
	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dec); err != nil {
		return nil, err
	}
	text := buf.String()
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

func TestChunkEncodeDecode50K(t *testing.T) {
	patterns := []string{"short", "medium", "long"}

	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			names := generateObjectNames(50000, pattern)

			// Compress
			start := time.Now()
			compressed, err := compressTextChunk(names)
			if err != nil {
				t.Fatalf("compress error: %v", err)
			}
			compressTime := time.Since(start)

			// Decompress
			start = time.Now()
			decoded, err := decompressTextChunk(compressed)
			if err != nil {
				t.Fatalf("decompress error: %v", err)
			}
			decompressTime := time.Since(start)

			// Verify
			if len(decoded) != len(names) {
				t.Fatalf("expected %d names, got %d", len(names), len(decoded))
			}
			for i, name := range names {
				if decoded[i] != name {
					t.Fatalf("mismatch at index %d: expected %q, got %q", i, name, decoded[i])
				}
			}

			rawSize := 0
			for _, n := range names {
				rawSize += len(n) + 1 // +1 for newline
			}

			t.Logf("Pattern: %s | Names: %d | Raw: %.1f MB | Compressed: %.1f KB (%.1f%% reduction)",
				pattern, len(names),
				float64(rawSize)/(1024*1024),
				float64(len(compressed))/1024,
				(1-float64(len(compressed))/float64(rawSize))*100)
			t.Logf("Compress: %v | Decompress: %v", compressTime, decompressTime)
		})
	}
}

func TestChunkEncodeDecode50K_RoundTrip(t *testing.T) {
	names := generateObjectNames(50000, "medium")

	compressed, err := compressTextChunk(names)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := decompressTextChunk(compressed)
	if err != nil {
		t.Fatal(err)
	}

	if len(decoded) != 50000 {
		t.Fatalf("expected 50000, got %d", len(decoded))
	}

	// Verify sort order preserved
	for i := 1; i < len(decoded); i++ {
		if decoded[i] < decoded[i-1] {
			t.Fatalf("sort order broken at index %d: %q < %q", i, decoded[i], decoded[i-1])
		}
	}
}

func TestDeduplicateAndSort(t *testing.T) {
	input := []string{"c", "a", "b", "a", "c", "d", "b"}
	result := deduplicateAndSort(input)
	expected := []string{"a", "b", "c", "d"}

	if len(result) != len(expected) {
		t.Fatalf("expected %d, got %d: %v", len(expected), len(result), result)
	}
	for i, v := range expected {
		if result[i] != v {
			t.Fatalf("index %d: expected %q, got %q", i, v, result[i])
		}
	}
}

func TestDeduplicateAndSort_Large(t *testing.T) {
	// 50K names with ~10% duplicates
	names := generateObjectNames(50000, "medium")
	// Add duplicates
	for i := 0; i < 5000; i++ {
		names = append(names, names[rand.Intn(50000)])
	}

	start := time.Now()
	result := deduplicateAndSort(names)
	dur := time.Since(start)

	if len(result) != 50000 {
		t.Fatalf("expected 50000 unique, got %d", len(result))
	}

	// Verify sorted and unique
	for i := 1; i < len(result); i++ {
		if result[i] <= result[i-1] {
			t.Fatalf("not sorted/unique at index %d", i)
		}
	}

	t.Logf("Deduplicate+sort 55K→50K: %v", dur)
}

func TestScaleSimulation(t *testing.T) {
	// Simulate what happens with 10M objects: 200 chunks of 50K each
	// We'll test with 5 chunks (250K names) to keep test fast

	numChunks := 5
	namesPerChunk := 50000
	totalNames := numChunks * namesPerChunk

	allNames := generateObjectNames(totalNames, "long")

	var totalCompressed int
	var chunks [][]byte

	// Write chunks
	writeStart := time.Now()
	for i := 0; i < numChunks; i++ {
		start := i * namesPerChunk
		end := start + namesPerChunk
		chunk := allNames[start:end]

		compressed, err := compressTextChunk(chunk)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, compressed)
		totalCompressed += len(compressed)
	}
	writeTime := time.Since(writeStart)

	// Read one chunk (simulates a paginated query)
	readStart := time.Now()
	decoded, err := decompressTextChunk(chunks[2]) // read chunk #2
	if err != nil {
		t.Fatal(err)
	}
	readTime := time.Since(readStart)

	if len(decoded) != namesPerChunk {
		t.Fatalf("expected %d, got %d", namesPerChunk, len(decoded))
	}

	// Stream all chunks (simulates stream-by-tag)
	streamStart := time.Now()
	streamTotal := 0
	for _, chunk := range chunks {
		names, err := decompressTextChunk(chunk)
		if err != nil {
			t.Fatal(err)
		}
		streamTotal += len(names)
	}
	streamTime := time.Since(streamStart)

	if streamTotal != totalNames {
		t.Fatalf("expected %d total, got %d", totalNames, streamTotal)
	}

	avgNameLen := 0
	for _, n := range allNames[:100] {
		avgNameLen += len(n)
	}
	avgNameLen /= 100

	t.Logf("=== Scale Simulation: %d objects across %d chunks ===", totalNames, numChunks)
	t.Logf("Avg name length: %d bytes", avgNameLen)
	t.Logf("Total raw: %.1f MB", float64(totalNames*avgNameLen)/(1024*1024))
	t.Logf("Total compressed: %.1f MB (%.1f%% reduction)", float64(totalCompressed)/(1024*1024),
		(1-float64(totalCompressed)/float64(totalNames*avgNameLen))*100)
	t.Logf("Per chunk compressed: %.1f KB", float64(totalCompressed/numChunks)/1024)
	t.Logf("Write %d chunks: %v", numChunks, writeTime)
	t.Logf("Read 1 chunk (paginated query): %v", readTime)
	t.Logf("Stream all %d chunks: %v", numChunks, streamTime)
	t.Logf("")
	t.Logf("=== Projected at 10M objects (200 chunks) ===")
	t.Logf("Total compressed: ~%.0f MB", float64(totalCompressed)/float64(numChunks)*200/(1024*1024))
	t.Logf("Paginated query: ~%v (1 chunk read)", readTime)
	t.Logf("Stream all: ~%v", streamTime*time.Duration(200/numChunks))
	t.Logf(".minio.sys usage: ~1 KB (meta only)")
}
