// Copyright (c) 2015-2024 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IamZoY/minio/internal/hash"
	"github.com/klauspost/compress/zstd"
)

// --- Constants ---

const (
	// tagIndexMetaPrefix is where the small meta file lives (in .minio.sys).
	tagIndexMetaPrefix = "event-tag-index"

	// tagIndexBucketPrefix is where chunk files live (in the user's bucket).
	tagIndexBucketPrefix = ".minio.tag-index"

	tagIndexMetaFile       = "_meta.json.zst"
	tagChunkSize           = 50000 // max object names per sorted chunk
	tagDeltaFlushThreshold = 5000  // flush delta when it reaches this size
	tagDeltaFlushInterval  = 30 * time.Second
	tagIndexChannelCap     = 100000
	tagCompactChannelCap   = 64
	tagUpdateWorkers       = 4
	tagChunkFormatV2       = "v2" // text+zstd in user bucket

	eventSentTagKey  = "EventSent"
	tagValueSuccess  = "Success"
	tagValueFailed   = "Failed"
	tagValueUntagged = "Untagged"
)

// --- Serializable types ---

// bucketTagMeta is the lightweight metadata kept in memory per bucket.
// Persisted as _meta.json.zst in .minio.sys (tiny, ~1-20 KB).
type bucketTagMeta struct {
	Version     uint32                         `json:"v"`
	Format      string                         `json:"fmt,omitempty"` // "" = old JSON in .minio.sys, "v2" = text in bucket
	LastRebuild time.Time                      `json:"lr"`
	Counts      map[string]map[string]int64    `json:"c"`  // tagKey->tagValue->count
	ChunkCounts map[string]map[string]int      `json:"cc"` // tagKey->tagValue->numChunks
	ChunkBounds map[string]map[string][]string `json:"cb"` // tagKey->tagValue->firstObjPerChunk
}

func newBucketTagMeta() *bucketTagMeta {
	return &bucketTagMeta{
		Version:     1,
		Format:      tagChunkFormatV2,
		Counts:      make(map[string]map[string]int64),
		ChunkCounts: make(map[string]map[string]int),
		ChunkBounds: make(map[string]map[string][]string),
	}
}

// tagDelta holds pending adds/removes for a specific tagKey/tagValue pair.
type tagDelta struct {
	Adds    map[string]struct{}
	Removes map[string]struct{}
}

func newTagDelta() *tagDelta {
	return &tagDelta{
		Adds:    make(map[string]struct{}),
		Removes: make(map[string]struct{}),
	}
}

func (d *tagDelta) size() int {
	return len(d.Adds) + len(d.Removes)
}

// --- Internal types ---

type tagIndexUpdate struct {
	bucket     string
	objectName string
	tagKey     string
	tagValue   string
}

type compactRequest struct {
	bucket   string
	tagKey   string
	tagValue string
}

// bucketDeltas holds all in-memory deltas for a single bucket.
type bucketDeltas struct {
	mu     sync.Mutex
	deltas map[string]map[string]*tagDelta // tagKey -> tagValue -> delta
}

func newBucketDeltas() *bucketDeltas {
	return &bucketDeltas{
		deltas: make(map[string]map[string]*tagDelta),
	}
}

func (bd *bucketDeltas) getDelta(tagKey, tagValue string) *tagDelta {
	if bd.deltas[tagKey] == nil {
		bd.deltas[tagKey] = make(map[string]*tagDelta)
	}
	if bd.deltas[tagKey][tagValue] == nil {
		bd.deltas[tagKey][tagValue] = newTagDelta()
	}
	return bd.deltas[tagKey][tagValue]
}

// --- TagIndexManager ---

// TagIndexManager manages sharded tag indexes for all buckets.
// Only lightweight metadata (counts + chunk boundaries) is kept in memory.
// Chunk files are stored as zstd-compressed text in the user's bucket under .minio.tag-index/.
// Only the tiny meta file (~1 KB) is stored in .minio.sys.
type TagIndexManager struct {
	meta      sync.Map // bucket -> *bucketTagMeta
	deltas    sync.Map // bucket -> *bucketDeltas
	updateCh  chan tagIndexUpdate
	compactCh chan compactRequest
}

var globalTagIndexManager *TagIndexManager

// StartTagIndexManager initializes and starts the background index worker.
func StartTagIndexManager(ctx context.Context, objAPI ObjectLayer) {
	mgr := &TagIndexManager{
		updateCh:  make(chan tagIndexUpdate, tagIndexChannelCap),
		compactCh: make(chan compactRequest, tagCompactChannelCap),
	}
	globalTagIndexManager = mgr

	// Load only lightweight meta files in background — does not block startup
	go mgr.loadAllMeta(ctx, objAPI)

	// Start update workers
	for i := 0; i < tagUpdateWorkers; i++ {
		go mgr.updateWorker(ctx, objAPI)
	}

	// Start compaction worker
	go mgr.compactionWorker(ctx, objAPI)
}

// SendIndexUpdate sends a non-blocking update to the index channel.
func SendIndexUpdate(bucket, objectName, tagKey, tagValue string) {
	if globalTagIndexManager == nil {
		return
	}
	select {
	case globalTagIndexManager.updateCh <- tagIndexUpdate{
		bucket:     bucket,
		objectName: objectName,
		tagKey:     tagKey,
		tagValue:   tagValue,
	}:
	default:
		internalLogIf(context.Background(),
			fmt.Errorf("event tag index update channel full, dropping update for %s/%s", bucket, objectName))
	}
}

// QueryTagIndex returns a paginated list of object names matching a tag key/value.
func QueryTagIndex(ctx context.Context, objAPI ObjectLayer, bucket, tagKey, tagValue, marker string, maxKeys int) (objects []string, nextMarker string, totalCount int64, isTruncated bool, err error) {
	if globalTagIndexManager == nil {
		return nil, "", 0, false, fmt.Errorf("tag index manager not initialized")
	}

	meta := globalTagIndexManager.getOrCreateMeta(bucket)
	totalCount = meta.Counts[tagKey][tagValue]
	if totalCount == 0 {
		return nil, "", 0, false, nil
	}

	numChunks := meta.ChunkCounts[tagKey][tagValue]
	bounds := meta.ChunkBounds[tagKey][tagValue]

	// Collect results from chunks + in-memory delta
	var result []string

	// Find starting chunk via binary search on bounds
	startChunk := 0
	if marker != "" && len(bounds) > 0 {
		startChunk = sort.Search(len(bounds), func(i int) bool {
			return bounds[i] > marker
		})
		if startChunk > 0 {
			startChunk--
		}
	}

	// Read chunks until we have enough results
	for chunkIdx := startChunk; chunkIdx < numChunks; chunkIdx++ {
		chunkData, readErr := readChunk(ctx, objAPI, bucket, tagKey, tagValue, chunkIdx)
		if readErr != nil {
			continue
		}

		for _, name := range chunkData {
			if marker != "" && name <= marker {
				continue
			}
			result = append(result, name)
			if len(result) > maxKeys {
				break
			}
		}
		if len(result) > maxKeys {
			break
		}
	}

	// Merge in-memory delta adds/removes
	result = globalTagIndexManager.applyDeltaToResults(bucket, tagKey, tagValue, marker, result)

	// Sort merged results
	sort.Strings(result)

	// Apply pagination
	if len(result) > maxKeys {
		isTruncated = true
		result = result[:maxKeys]
	}

	if isTruncated && len(result) > 0 {
		nextMarker = result[len(result)-1]
	}

	return result, nextMarker, totalCount, isTruncated, nil
}

// RebuildTagIndex performs a streaming scan of the bucket and rebuilds the index
// using chunk files. Does not hold locks that block queries during the scan.
func RebuildTagIndex(ctx context.Context, objAPI ObjectLayer, bucket string) (map[string]map[string]int64, error) {
	if globalTagIndexManager == nil {
		return nil, fmt.Errorf("tag index manager not initialized")
	}

	// Collect all objects grouped by tagKey/tagValue
	collectors := make(map[string]map[string][]string)
	marker := ""

	for {
		result, err := objAPI.ListObjectsV2(ctx, bucket, "", marker, "", 1000, false, "")
		if err != nil {
			return nil, fmt.Errorf("rebuild scan failed: %w", err)
		}

		for _, obj := range result.Objects {
			// Skip our own index files
			if strings.HasPrefix(obj.Name, tagIndexBucketPrefix+"/") {
				continue
			}
			if obj.UserTags == "" {
				addToCollectors(collectors, eventSentTagKey, tagValueUntagged, obj.Name)
				continue
			}
			parsedTags, err := url.ParseQuery(obj.UserTags)
			if err != nil || len(parsedTags) == 0 {
				addToCollectors(collectors, eventSentTagKey, tagValueUntagged, obj.Name)
				continue
			}
			for key, values := range parsedTags {
				for _, val := range values {
					addToCollectors(collectors, key, val, obj.Name)
				}
			}
		}

		if !result.IsTruncated {
			break
		}
		marker = result.NextContinuationToken
	}

	// Build new meta
	newMeta := newBucketTagMeta()
	newMeta.LastRebuild = time.Now().UTC()

	// Write sorted chunks for each tagKey/tagValue
	for tagKey, tagValues := range collectors {
		if newMeta.Counts[tagKey] == nil {
			newMeta.Counts[tagKey] = make(map[string]int64)
			newMeta.ChunkCounts[tagKey] = make(map[string]int)
			newMeta.ChunkBounds[tagKey] = make(map[string][]string)
		}
		for tagValue, names := range tagValues {
			sort.Strings(names)
			newMeta.Counts[tagKey][tagValue] = int64(len(names))

			numChunks := 0
			var chunkBounds []string
			for i := 0; i < len(names); i += tagChunkSize {
				end := i + tagChunkSize
				if end > len(names) {
					end = len(names)
				}
				chunk := names[i:end]
				chunkBounds = append(chunkBounds, chunk[0])

				if err := writeChunk(ctx, objAPI, bucket, tagKey, tagValue, numChunks, chunk); err != nil {
					return nil, fmt.Errorf("failed to write chunk: %w", err)
				}
				numChunks++
			}

			newMeta.ChunkCounts[tagKey][tagValue] = numChunks
			newMeta.ChunkBounds[tagKey][tagValue] = chunkBounds

			// Clean up old extra chunks
			globalTagIndexManager.cleanOldChunks(ctx, objAPI, bucket, tagKey, tagValue, numChunks)
		}
	}

	// Save meta to .minio.sys
	if err := writeMetaFile(ctx, objAPI, bucket, newMeta); err != nil {
		return nil, fmt.Errorf("failed to save meta: %w", err)
	}

	// Update in-memory state
	globalTagIndexManager.meta.Store(bucket, newMeta)
	globalTagIndexManager.deltas.Store(bucket, newBucketDeltas())

	// Delete old-format files if they exist (migration)
	deleteConfig(ctx, objAPI, path.Join(tagIndexMetaPrefix, bucket+".json"))

	return newMeta.Counts, nil
}

// StreamTagIndex streams all object names matching a tag key/value through a callback,
// reading one chunk at a time to keep memory bounded. The callback receives batches of
// sorted object names. Returns the total count of objects streamed.
func StreamTagIndex(ctx context.Context, objAPI ObjectLayer, bucket, tagKey, tagValue string, fn func(names []string) error) (int64, error) {
	if globalTagIndexManager == nil {
		return 0, fmt.Errorf("tag index manager not initialized")
	}

	meta := globalTagIndexManager.getOrCreateMeta(bucket)
	numChunks := meta.ChunkCounts[tagKey][tagValue]

	// Snapshot delta adds/removes
	var deltaAdds map[string]struct{}
	var deltaRemoves map[string]struct{}
	if val, ok := globalTagIndexManager.deltas.Load(bucket); ok {
		if bd, ok := val.(*bucketDeltas); ok {
			bd.mu.Lock()
			if d := bd.deltas[tagKey][tagValue]; d != nil {
				deltaAdds = make(map[string]struct{}, len(d.Adds))
				for k := range d.Adds {
					deltaAdds[k] = struct{}{}
				}
				deltaRemoves = make(map[string]struct{}, len(d.Removes))
				for k := range d.Removes {
					deltaRemoves[k] = struct{}{}
				}
			}
			bd.mu.Unlock()
		}
	}

	var total int64

	for i := 0; i < numChunks; i++ {
		chunkData, err := readChunk(ctx, objAPI, bucket, tagKey, tagValue, i)
		if err != nil {
			continue
		}

		if len(deltaRemoves) > 0 {
			filtered := chunkData[:0]
			for _, name := range chunkData {
				if _, removed := deltaRemoves[name]; !removed {
					filtered = append(filtered, name)
				}
			}
			chunkData = filtered
		}

		total += int64(len(chunkData))
		if err := fn(chunkData); err != nil {
			return total, err
		}
	}

	// Stream delta adds
	if len(deltaAdds) > 0 {
		extraNames := make([]string, 0, len(deltaAdds))
		for name := range deltaAdds {
			extraNames = append(extraNames, name)
		}
		sort.Strings(extraNames)
		total += int64(len(extraNames))
		if err := fn(extraNames); err != nil {
			return total, err
		}
	}

	return total, nil
}

// RemoveBucketIndex removes the in-memory index and all persisted files for a bucket.
func RemoveBucketIndex(ctx context.Context, objAPI ObjectLayer, bucket string) {
	if globalTagIndexManager == nil {
		return
	}

	// Delete chunk files from user bucket
	val, ok := globalTagIndexManager.meta.Load(bucket)
	if ok {
		if meta, ok2 := val.(*bucketTagMeta); ok2 {
			for tagKey, tagValues := range meta.ChunkCounts {
				for tagValue, numChunks := range tagValues {
					for i := 0; i < numChunks; i++ {
						tagIndexDelete(ctx, objAPI, bucket, tagChunkObjectPath(tagKey, tagValue, i))
					}
				}
			}
		}
	}

	globalTagIndexManager.meta.Delete(bucket)
	globalTagIndexManager.deltas.Delete(bucket)

	// Delete meta from .minio.sys
	deleteConfig(ctx, objAPI, tagMetaPath(bucket))

	// Delete old format files
	deleteConfig(ctx, objAPI, path.Join(tagIndexMetaPrefix, bucket+".json"))
}

// --- Internal methods ---

func (mgr *TagIndexManager) getOrCreateMeta(bucket string) *bucketTagMeta {
	val, ok := mgr.meta.Load(bucket)
	if ok {
		if m, ok2 := val.(*bucketTagMeta); ok2 {
			return m
		}
	}
	m := newBucketTagMeta()
	actual, _ := mgr.meta.LoadOrStore(bucket, m)
	if result, ok2 := actual.(*bucketTagMeta); ok2 {
		return result
	}
	return m
}

func (mgr *TagIndexManager) getOrCreateDeltas(bucket string) *bucketDeltas {
	val, ok := mgr.deltas.Load(bucket)
	if ok {
		if bd, ok2 := val.(*bucketDeltas); ok2 {
			return bd
		}
	}
	bd := newBucketDeltas()
	actual, _ := mgr.deltas.LoadOrStore(bucket, bd)
	if result, ok2 := actual.(*bucketDeltas); ok2 {
		return result
	}
	return bd
}

func (mgr *TagIndexManager) updateWorker(ctx context.Context, objAPI ObjectLayer) {
	flushTicker := time.NewTicker(tagDeltaFlushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case update, ok := <-mgr.updateCh:
			if !ok {
				return
			}
			mgr.applyUpdate(update)

		case <-flushTicker.C:
			mgr.flushAllDeltas(ctx, objAPI)

		case <-ctx.Done():
			for {
				select {
				case update := <-mgr.updateCh:
					mgr.applyUpdate(update)
				default:
					mgr.flushAllDeltas(ctx, objAPI)
					return
				}
			}
		}
	}
}

func (mgr *TagIndexManager) applyUpdate(update tagIndexUpdate) {
	bd := mgr.getOrCreateDeltas(update.bucket)
	meta := mgr.getOrCreateMeta(update.bucket)

	bd.mu.Lock()

	// Remove from old tag values under same key
	if keyDeltas, ok := bd.deltas[update.tagKey]; ok {
		for tv, delta := range keyDeltas {
			if tv == update.tagValue {
				continue
			}
			if _, wasAdded := delta.Adds[update.objectName]; wasAdded {
				delete(delta.Adds, update.objectName)
				delta.Removes[update.objectName] = struct{}{}
			} else {
				delta.Removes[update.objectName] = struct{}{}
			}
		}
	}

	// Add to target tag value
	d := bd.getDelta(update.tagKey, update.tagValue)
	delete(d.Removes, update.objectName)
	d.Adds[update.objectName] = struct{}{}

	needsCompact := d.size() >= tagDeltaFlushThreshold
	bd.mu.Unlock()

	// Update counts (approximate)
	if meta.Counts[update.tagKey] == nil {
		meta.Counts[update.tagKey] = make(map[string]int64)
	}
	meta.Counts[update.tagKey][update.tagValue]++

	if needsCompact {
		select {
		case mgr.compactCh <- compactRequest{
			bucket:   update.bucket,
			tagKey:   update.tagKey,
			tagValue: update.tagValue,
		}:
		default:
		}
	}
}

func (mgr *TagIndexManager) applyDeltaToResults(bucket, tagKey, tagValue, marker string, chunkResults []string) []string {
	val, ok := mgr.deltas.Load(bucket)
	if !ok {
		return chunkResults
	}
	bd, ok := val.(*bucketDeltas)
	if !ok {
		return chunkResults
	}

	bd.mu.Lock()
	defer bd.mu.Unlock()

	d := bd.deltas[tagKey][tagValue]
	if d == nil {
		return chunkResults
	}

	resultSet := make(map[string]struct{}, len(chunkResults))
	for _, name := range chunkResults {
		resultSet[name] = struct{}{}
	}

	for name := range d.Removes {
		delete(resultSet, name)
	}
	for name := range d.Adds {
		if marker != "" && name <= marker {
			continue
		}
		resultSet[name] = struct{}{}
	}

	result := make([]string, 0, len(resultSet))
	for name := range resultSet {
		result = append(result, name)
	}
	return result
}

func (mgr *TagIndexManager) compactionWorker(ctx context.Context, objAPI ObjectLayer) {
	for {
		select {
		case req, ok := <-mgr.compactCh:
			if !ok {
				return
			}
			mgr.compactTagValue(ctx, objAPI, req.bucket, req.tagKey, req.tagValue)

		case <-ctx.Done():
			return
		}
	}
}

func (mgr *TagIndexManager) compactTagValue(ctx context.Context, objAPI ObjectLayer, bucket, tagKey, tagValue string) {
	bd := mgr.getOrCreateDeltas(bucket)
	meta := mgr.getOrCreateMeta(bucket)

	bd.mu.Lock()
	d := bd.deltas[tagKey][tagValue]
	if d == nil || d.size() == 0 {
		bd.mu.Unlock()
		return
	}
	adds := d.Adds
	removes := d.Removes
	bd.deltas[tagKey][tagValue] = newTagDelta()
	bd.mu.Unlock()

	// Read all existing chunks
	numChunks := meta.ChunkCounts[tagKey][tagValue]
	var allNames []string

	for i := 0; i < numChunks; i++ {
		chunkData, err := readChunk(ctx, objAPI, bucket, tagKey, tagValue, i)
		if err != nil {
			continue
		}
		allNames = append(allNames, chunkData...)
	}

	// Apply removes
	if len(removes) > 0 {
		filtered := allNames[:0]
		for _, name := range allNames {
			if _, removed := removes[name]; !removed {
				filtered = append(filtered, name)
			}
		}
		allNames = filtered
	}

	// Apply adds
	for name := range adds {
		allNames = append(allNames, name)
	}

	allNames = deduplicateAndSort(allNames)

	// Write new chunks
	newNumChunks := 0
	var newBounds []string

	if len(allNames) > 0 {
		for i := 0; i < len(allNames); i += tagChunkSize {
			end := i + tagChunkSize
			if end > len(allNames) {
				end = len(allNames)
			}
			chunk := allNames[i:end]
			newBounds = append(newBounds, chunk[0])

			if err := writeChunk(ctx, objAPI, bucket, tagKey, tagValue, newNumChunks, chunk); err != nil {
				internalLogIf(ctx, fmt.Errorf("compaction: failed to write chunk: %w", err))
				return
			}
			newNumChunks++
		}
	}

	mgr.cleanOldChunks(ctx, objAPI, bucket, tagKey, tagValue, newNumChunks)

	// Update meta
	if meta.Counts[tagKey] == nil {
		meta.Counts[tagKey] = make(map[string]int64)
		meta.ChunkCounts[tagKey] = make(map[string]int)
		meta.ChunkBounds[tagKey] = make(map[string][]string)
	}
	meta.Counts[tagKey][tagValue] = int64(len(allNames))
	meta.ChunkCounts[tagKey][tagValue] = newNumChunks
	meta.ChunkBounds[tagKey][tagValue] = newBounds

	if err := writeMetaFile(ctx, objAPI, bucket, meta); err != nil {
		internalLogIf(ctx, fmt.Errorf("compaction: failed to save meta: %w", err))
	}
}

func (mgr *TagIndexManager) cleanOldChunks(ctx context.Context, objAPI ObjectLayer, bucket, tagKey, tagValue string, keepCount int) {
	for i := keepCount; i < keepCount+100; i++ {
		chunkPath := tagChunkObjectPath(tagKey, tagValue, i)
		if err := tagIndexDelete(ctx, objAPI, bucket, chunkPath); err != nil {
			break
		}
	}
}

func (mgr *TagIndexManager) flushAllDeltas(ctx context.Context, objAPI ObjectLayer) {
	mgr.deltas.Range(func(key, value any) bool {
		bucket, ok := key.(string)
		if !ok {
			return true
		}
		bd, ok := value.(*bucketDeltas)
		if !ok {
			return true
		}

		bd.mu.Lock()
		for tagKey, tagValues := range bd.deltas {
			for tagValue, d := range tagValues {
				if d.size() > 0 {
					select {
					case mgr.compactCh <- compactRequest{
						bucket:   bucket,
						tagKey:   tagKey,
						tagValue: tagValue,
					}:
					default:
					}
				}
			}
		}
		bd.mu.Unlock()
		return true
	})
}

func (mgr *TagIndexManager) loadAllMeta(ctx context.Context, objAPI ObjectLayer) {
	buckets, err := objAPI.ListBuckets(ctx, BucketOptions{})
	if err != nil {
		internalLogIf(ctx, fmt.Errorf("failed to list buckets for tag index loading: %w", err))
		return
	}

	for _, bucket := range buckets {
		mgr.loadBucketMeta(ctx, objAPI, bucket.Name)
	}
}

func (mgr *TagIndexManager) loadBucketMeta(ctx context.Context, objAPI ObjectLayer, bucket string) {
	// Try loading meta from .minio.sys
	metaPath := tagMetaPath(bucket)
	meta, err := readMetaFile(ctx, objAPI, metaPath)
	if err == nil {
		if meta.Format == "" {
			// Old format: chunks in .minio.sys as JSON. Migrate in background.
			go mgr.migrateV1ToV2(ctx, objAPI, bucket, meta)
		} else {
			mgr.meta.Store(bucket, meta)
		}
		return
	}

	// Try old single-JSON format (very old)
	mgr.migrateOldFormat(ctx, objAPI, bucket)
}

func (mgr *TagIndexManager) migrateV1ToV2(ctx context.Context, objAPI ObjectLayer, bucket string, oldMeta *bucketTagMeta) {
	newMeta := newBucketTagMeta()
	newMeta.LastRebuild = oldMeta.LastRebuild
	newMeta.Counts = oldMeta.Counts
	newMeta.ChunkCounts = make(map[string]map[string]int, len(oldMeta.ChunkCounts))
	newMeta.ChunkBounds = make(map[string]map[string][]string, len(oldMeta.ChunkBounds))

	for tagKey, tagValues := range oldMeta.ChunkCounts {
		newMeta.ChunkCounts[tagKey] = make(map[string]int, len(tagValues))
		newMeta.ChunkBounds[tagKey] = make(map[string][]string)
		for tagValue, numChunks := range tagValues {
			var newBounds []string
			newNumChunks := 0

			for i := 0; i < numChunks; i++ {
				// Read old chunk from .minio.sys (JSON format)
				oldChunkPath := path.Join(tagIndexMetaPrefix, bucket, tagKey, tagValue, fmt.Sprintf("chunk-%06d.json.zst", i))
				oldData, err := readZstdJSON[[]string](ctx, objAPI, oldChunkPath)
				if err != nil {
					continue
				}

				if len(oldData) > 0 {
					newBounds = append(newBounds, oldData[0])
				}

				// Write new chunk to user bucket (text format)
				if err := writeChunk(ctx, objAPI, bucket, tagKey, tagValue, newNumChunks, oldData); err != nil {
					internalLogIf(ctx, fmt.Errorf("migration v1→v2: failed to write chunk for bucket %s: %w", bucket, err))
					return
				}
				newNumChunks++

				// Delete old chunk from .minio.sys
				deleteConfig(ctx, objAPI, oldChunkPath)
			}

			newMeta.ChunkCounts[tagKey][tagValue] = newNumChunks
			newMeta.ChunkBounds[tagKey][tagValue] = newBounds
		}
	}

	// Save updated meta
	if err := writeMetaFile(ctx, objAPI, bucket, newMeta); err != nil {
		internalLogIf(ctx, fmt.Errorf("migration v1→v2: failed to save meta for bucket %s: %w", bucket, err))
		return
	}

	mgr.meta.Store(bucket, newMeta)
	internalLogIf(ctx, fmt.Errorf("migrated tag index for bucket %s from v1 (JSON in .minio.sys) to v2 (text in bucket)", bucket))
}

func (mgr *TagIndexManager) migrateOldFormat(ctx context.Context, objAPI ObjectLayer, bucket string) {
	oldPath := path.Join(tagIndexMetaPrefix, bucket+".json")
	data, err := readConfig(ctx, objAPI, oldPath)
	if err != nil {
		return
	}

	var oldData map[string]map[string][]string
	if err := json.Unmarshal(data, &oldData); err != nil {
		var veryOldData map[string][]string
		if err2 := json.Unmarshal(data, &veryOldData); err2 != nil {
			internalLogIf(ctx, fmt.Errorf("failed to parse old tag index for bucket %s: %w", bucket, err))
			return
		}
		oldData = map[string]map[string][]string{
			eventSentTagKey: veryOldData,
		}
	}

	newMeta := newBucketTagMeta()

	for tagKey, tagValues := range oldData {
		if newMeta.Counts[tagKey] == nil {
			newMeta.Counts[tagKey] = make(map[string]int64)
			newMeta.ChunkCounts[tagKey] = make(map[string]int)
			newMeta.ChunkBounds[tagKey] = make(map[string][]string)
		}

		for tagValue, names := range tagValues {
			sort.Strings(names)
			newMeta.Counts[tagKey][tagValue] = int64(len(names))

			numChunks := 0
			var bounds []string
			for i := 0; i < len(names); i += tagChunkSize {
				end := i + tagChunkSize
				if end > len(names) {
					end = len(names)
				}
				chunk := names[i:end]
				bounds = append(bounds, chunk[0])

				if err := writeChunk(ctx, objAPI, bucket, tagKey, tagValue, numChunks, chunk); err != nil {
					internalLogIf(ctx, fmt.Errorf("migration: failed to write chunk for bucket %s: %w", bucket, err))
					return
				}
				numChunks++
			}

			newMeta.ChunkCounts[tagKey][tagValue] = numChunks
			newMeta.ChunkBounds[tagKey][tagValue] = bounds
		}
	}

	if err := writeMetaFile(ctx, objAPI, bucket, newMeta); err != nil {
		internalLogIf(ctx, fmt.Errorf("migration: failed to save meta for bucket %s: %w", bucket, err))
		return
	}

	mgr.meta.Store(bucket, newMeta)
	deleteConfig(ctx, objAPI, oldPath)
	internalLogIf(ctx, fmt.Errorf("migrated tag index for bucket %s to v2 sharded format", bucket))
}

// --- Path helpers ---

// tagMetaPath returns the path for the meta file in .minio.sys.
func tagMetaPath(bucket string) string {
	return path.Join(tagIndexMetaPrefix, bucket, tagIndexMetaFile)
}

// tagChunkObjectPath returns the object path for a chunk within the user's bucket.
func tagChunkObjectPath(tagKey, tagValue string, chunkIdx int) string {
	return path.Join(tagIndexBucketPrefix, tagKey, tagValue, fmt.Sprintf("chunk-%06d.txt.zst", chunkIdx))
}

// --- Bucket I/O helpers (chunks stored in user bucket) ---

func tagIndexPut(ctx context.Context, objAPI ObjectLayer, bucket, objectPath string, data []byte) error {
	hashReader, err := hash.NewReader(ctx, bytes.NewReader(data), int64(len(data)), "", getSHA256Hash(data), int64(len(data)))
	if err != nil {
		return err
	}
	_, err = objAPI.PutObject(ctx, bucket, objectPath, NewPutObjReader(hashReader), ObjectOptions{})
	return err
}

func tagIndexGet(ctx context.Context, objAPI ObjectLayer, bucket, objectPath string) ([]byte, error) {
	r, err := objAPI.GetObjectNInfo(ctx, bucket, objectPath, nil, http.Header{}, ObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func tagIndexDelete(ctx context.Context, objAPI ObjectLayer, bucket, objectPath string) error {
	_, err := objAPI.DeleteObject(ctx, bucket, objectPath, ObjectOptions{
		DeletePrefix:       true,
		DeletePrefixObject: true,
	})
	if err != nil && isErrObjectNotFound(err) {
		return errConfigNotFound
	}
	return err
}

// --- Chunk I/O (newline-delimited text + zstd, stored in user bucket) ---

func writeChunk(ctx context.Context, objAPI ObjectLayer, bucket, tagKey, tagValue string, chunkIdx int, names []string) error {
	raw := []byte(strings.Join(names, "\n"))

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithWindowSize(1<<20))
	if err != nil {
		return fmt.Errorf("zstd writer error: %w", err)
	}
	if _, err := enc.Write(raw); err != nil {
		return fmt.Errorf("zstd write error: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close error: %w", err)
	}

	chunkPath := tagChunkObjectPath(tagKey, tagValue, chunkIdx)
	return tagIndexPut(ctx, objAPI, bucket, chunkPath, buf.Bytes())
}

func readChunk(ctx context.Context, objAPI ObjectLayer, bucket, tagKey, tagValue string, chunkIdx int) ([]string, error) {
	chunkPath := tagChunkObjectPath(tagKey, tagValue, chunkIdx)
	data, err := tagIndexGet(ctx, objAPI, bucket, chunkPath)
	if err != nil {
		return nil, err
	}

	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("zstd reader error: %w", err)
	}
	defer dec.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dec); err != nil {
		return nil, fmt.Errorf("zstd decompress error: %w", err)
	}

	text := buf.String()
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// --- Meta I/O (JSON + zstd, stored in .minio.sys) ---

func writeMetaFile(ctx context.Context, objAPI ObjectLayer, bucket string, meta *bucketTagMeta) error {
	return writeZstdJSON(ctx, objAPI, tagMetaPath(bucket), meta)
}

func readMetaFile(ctx context.Context, objAPI ObjectLayer, metaPath string) (*bucketTagMeta, error) {
	meta, err := readZstdJSON[bucketTagMeta](ctx, objAPI, metaPath)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

func writeZstdJSON(ctx context.Context, objAPI ObjectLayer, filePath string, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithWindowSize(1<<20))
	if err != nil {
		return fmt.Errorf("zstd writer error: %w", err)
	}
	if _, err := enc.Write(jsonData); err != nil {
		return fmt.Errorf("zstd write error: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close error: %w", err)
	}

	return saveConfig(ctx, objAPI, filePath, buf.Bytes())
}

func readZstdJSON[T any](ctx context.Context, objAPI ObjectLayer, filePath string) (T, error) {
	var zero T
	data, err := readConfig(ctx, objAPI, filePath)
	if err != nil {
		return zero, err
	}

	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return zero, fmt.Errorf("zstd reader error: %w", err)
	}
	defer dec.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dec); err != nil {
		return zero, fmt.Errorf("zstd decompress error: %w", err)
	}

	var result T
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		return zero, fmt.Errorf("unmarshal error: %w", err)
	}
	return result, nil
}

// --- Utility helpers ---

func addToCollectors(collectors map[string]map[string][]string, tagKey, tagValue, objectName string) {
	if collectors[tagKey] == nil {
		collectors[tagKey] = make(map[string][]string)
	}
	collectors[tagKey][tagValue] = append(collectors[tagKey][tagValue], objectName)
}

func deduplicateAndSort(names []string) []string {
	if len(names) == 0 {
		return names
	}
	sort.Strings(names)
	j := 0
	for i := 1; i < len(names); i++ {
		if names[i] != names[j] {
			j++
			names[j] = names[i]
		}
	}
	return names[:j+1]
}
