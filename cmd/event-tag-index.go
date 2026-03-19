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
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sync"
	"time"
)

const (
	eventTagIndexPrefix   = "event-tag-index"
	tagIndexFlushInterval = 5 * time.Second
	tagIndexChannelCap    = 10000
	eventSentTagKey       = "EventSent"
	tagValueSuccess       = "Success"
	tagValueFailed        = "Failed"
	tagValueUntagged      = "Untagged"
)

type tagIndexUpdate struct {
	bucket     string
	objectName string
	tagKey     string
	tagValue   string
}

// BucketTagIndex holds the tag index data for a single bucket.
// Structured as tagKey -> tagValue -> set of objectNames.
type BucketTagIndex struct {
	mu   sync.RWMutex
	data map[string]map[string]map[string]struct{}
}

func newBucketTagIndex() *BucketTagIndex {
	return &BucketTagIndex{
		data: make(map[string]map[string]map[string]struct{}),
	}
}

// TagIndexManager manages tag indexes for all buckets.
type TagIndexManager struct {
	indexes   sync.Map   // bucket -> *BucketTagIndex
	updateCh  chan tagIndexUpdate
	dirty     sync.Map   // bucket -> bool
	flushMu   sync.Mutex
	rebuildMu sync.Mutex // serializes rebuild vs live updates
}

var globalTagIndexManager *TagIndexManager

// StartTagIndexManager initializes and starts the background index worker.
func StartTagIndexManager(ctx context.Context, objAPI ObjectLayer) {
	mgr := &TagIndexManager{
		updateCh: make(chan tagIndexUpdate, tagIndexChannelCap),
	}
	globalTagIndexManager = mgr

	mgr.loadAllIndexes(ctx, objAPI)

	go mgr.backgroundWorker(ctx, objAPI)
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

// QueryTagIndex returns the list of object names matching a tag key and value in a bucket.
func QueryTagIndex(bucket, tagKey, tagValue string) []string {
	if globalTagIndexManager == nil {
		return nil
	}
	val, ok := globalTagIndexManager.indexes.Load(bucket)
	if !ok {
		return nil
	}
	idx := val.(*BucketTagIndex)
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	keyMap := idx.data[tagKey]
	if keyMap == nil {
		return nil
	}
	objects := keyMap[tagValue]
	if len(objects) == 0 {
		return nil
	}
	result := make([]string, 0, len(objects))
	for name := range objects {
		result = append(result, name)
	}
	return result
}

// RebuildTagIndex performs a full scan of the bucket and rebuilds the index.
// Indexes ALL tag key/value pairs found on each object.
func RebuildTagIndex(ctx context.Context, objAPI ObjectLayer, bucket string) (map[string]map[string]int, error) {
	if globalTagIndexManager == nil {
		return nil, fmt.Errorf("tag index manager not initialized")
	}

	globalTagIndexManager.rebuildMu.Lock()
	defer globalTagIndexManager.rebuildMu.Unlock()

	newIndex := newBucketTagIndex()
	marker := ""

	for {
		result, err := objAPI.ListObjectsV2(ctx, bucket, "", marker, "", 1000, false, "")
		if err != nil {
			return nil, err
		}

		for _, obj := range result.Objects {
			if obj.UserTags == "" {
				addToIndex(newIndex, eventSentTagKey, tagValueUntagged, obj.Name)
				continue
			}
			parsedTags, err := url.ParseQuery(obj.UserTags)
			if err != nil {
				addToIndex(newIndex, eventSentTagKey, tagValueUntagged, obj.Name)
				continue
			}
			if len(parsedTags) == 0 {
				addToIndex(newIndex, eventSentTagKey, tagValueUntagged, obj.Name)
				continue
			}
			for key, values := range parsedTags {
				for _, val := range values {
					addToIndex(newIndex, key, val, obj.Name)
				}
			}
		}

		if !result.IsTruncated {
			break
		}
		marker = result.NextContinuationToken
	}

	globalTagIndexManager.indexes.Store(bucket, newIndex)
	globalTagIndexManager.dirty.Store(bucket, true)

	globalTagIndexManager.flushBucket(ctx, objAPI, bucket)

	// Build counts: tagKey -> tagValue -> count
	counts := make(map[string]map[string]int)
	for tagKey, tagValues := range newIndex.data {
		counts[tagKey] = make(map[string]int)
		for tagValue, objects := range tagValues {
			counts[tagKey][tagValue] = len(objects)
		}
	}

	return counts, nil
}

// addToIndex adds an object to a specific tagKey/tagValue in a BucketTagIndex.
func addToIndex(idx *BucketTagIndex, tagKey, tagValue, objectName string) {
	if idx.data[tagKey] == nil {
		idx.data[tagKey] = make(map[string]map[string]struct{})
	}
	if idx.data[tagKey][tagValue] == nil {
		idx.data[tagKey][tagValue] = make(map[string]struct{})
	}
	idx.data[tagKey][tagValue][objectName] = struct{}{}
}

// RemoveBucketIndex removes the in-memory index and persisted file for a bucket.
func RemoveBucketIndex(ctx context.Context, objAPI ObjectLayer, bucket string) {
	if globalTagIndexManager == nil {
		return
	}
	globalTagIndexManager.indexes.Delete(bucket)
	globalTagIndexManager.dirty.Delete(bucket)
	indexPath := path.Join(eventTagIndexPrefix, bucket+".json")
	deleteConfig(ctx, objAPI, indexPath)
}

func (mgr *TagIndexManager) getOrCreateIndex(bucket string) *BucketTagIndex {
	val, ok := mgr.indexes.Load(bucket)
	if ok {
		return val.(*BucketTagIndex)
	}
	idx := newBucketTagIndex()
	actual, _ := mgr.indexes.LoadOrStore(bucket, idx)
	return actual.(*BucketTagIndex)
}

func (mgr *TagIndexManager) applyUpdate(update tagIndexUpdate) {
	idx := mgr.getOrCreateIndex(update.bucket)
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Remove this object from all values under the same tag key (O(values) per key)
	if keyMap := idx.data[update.tagKey]; keyMap != nil {
		for _, objects := range keyMap {
			delete(objects, update.objectName)
		}
	}

	// Add to the correct tagKey/tagValue
	if idx.data[update.tagKey] == nil {
		idx.data[update.tagKey] = make(map[string]map[string]struct{})
	}
	if idx.data[update.tagKey][update.tagValue] == nil {
		idx.data[update.tagKey][update.tagValue] = make(map[string]struct{})
	}
	idx.data[update.tagKey][update.tagValue][update.objectName] = struct{}{}
	mgr.dirty.Store(update.bucket, true)
}

func (mgr *TagIndexManager) backgroundWorker(ctx context.Context, objAPI ObjectLayer) {
	flushTicker := time.NewTicker(tagIndexFlushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case update, ok := <-mgr.updateCh:
			if !ok {
				mgr.flushAll(context.Background(), objAPI)
				return
			}
			mgr.rebuildMu.Lock()
			mgr.applyUpdate(update)
			mgr.rebuildMu.Unlock()

		case <-flushTicker.C:
			mgr.flushAll(ctx, objAPI)

		case <-ctx.Done():
			for {
				select {
				case update := <-mgr.updateCh:
					mgr.applyUpdate(update)
				default:
					mgr.flushAll(context.Background(), objAPI)
					return
				}
			}
		}
	}
}

func (mgr *TagIndexManager) flushAll(ctx context.Context, objAPI ObjectLayer) {
	mgr.dirty.Range(func(key, value any) bool {
		bucket := key.(string)
		isDirty := value.(bool)
		if isDirty {
			mgr.flushBucket(ctx, objAPI, bucket)
		}
		return true
	})
}

func (mgr *TagIndexManager) flushBucket(ctx context.Context, objAPI ObjectLayer, bucket string) {
	val, ok := mgr.indexes.Load(bucket)
	if !ok {
		return
	}
	idx := val.(*BucketTagIndex)

	// Serialize: tagKey -> tagValue -> []objectName
	idx.mu.RLock()
	dataCopy := make(map[string]map[string][]string, len(idx.data))
	for tagKey, tagValues := range idx.data {
		dataCopy[tagKey] = make(map[string][]string, len(tagValues))
		for tagValue, objects := range tagValues {
			names := make([]string, 0, len(objects))
			for name := range objects {
				names = append(names, name)
			}
			dataCopy[tagKey][tagValue] = names
		}
	}
	idx.mu.RUnlock()

	data, err := json.Marshal(dataCopy)
	if err != nil {
		internalLogIf(ctx, fmt.Errorf("failed to marshal tag index for bucket %s: %w", bucket, err))
		return
	}

	indexPath := path.Join(eventTagIndexPrefix, bucket+".json")

	mgr.flushMu.Lock()
	defer mgr.flushMu.Unlock()

	if err := saveConfig(ctx, objAPI, indexPath, data); err != nil {
		internalLogIf(ctx, fmt.Errorf("failed to save tag index for bucket %s: %w", bucket, err))
		return
	}

	mgr.dirty.Store(bucket, false)
}

func (mgr *TagIndexManager) loadAllIndexes(ctx context.Context, objAPI ObjectLayer) {
	buckets, err := objAPI.ListBuckets(ctx, BucketOptions{})
	if err != nil {
		internalLogIf(ctx, fmt.Errorf("failed to list buckets for tag index loading: %w", err))
		return
	}

	for _, bucket := range buckets {
		mgr.loadBucketIndex(ctx, objAPI, bucket.Name)
	}
}

func (mgr *TagIndexManager) loadBucketIndex(ctx context.Context, objAPI ObjectLayer, bucket string) {
	indexPath := path.Join(eventTagIndexPrefix, bucket+".json")
	data, err := readConfig(ctx, objAPI, indexPath)
	if err != nil {
		return
	}

	// Try new format: tagKey -> tagValue -> []objectName
	var newFormatData map[string]map[string][]string
	if err := json.Unmarshal(data, &newFormatData); err != nil {
		internalLogIf(ctx, fmt.Errorf("failed to parse tag index for bucket %s: %w", bucket, err))
		return
	}

	idx := newBucketTagIndex()

	// Detect format: if any value is a nested map, it's new format.
	// Otherwise it's old format (tagValue -> []objectName) which we migrate.
	isNewFormat := false
	for _, inner := range newFormatData {
		if inner != nil {
			isNewFormat = true
			break
		}
	}

	if isNewFormat {
		for tagKey, tagValues := range newFormatData {
			idx.data[tagKey] = make(map[string]map[string]struct{}, len(tagValues))
			for tagValue, names := range tagValues {
				set := make(map[string]struct{}, len(names))
				for _, name := range names {
					set[name] = struct{}{}
				}
				idx.data[tagKey][tagValue] = set
			}
		}
	} else {
		// Old format migration: treat keys as tagValues under EventSent
		var oldData map[string][]string
		if err := json.Unmarshal(data, &oldData); err == nil {
			idx.data[eventSentTagKey] = make(map[string]map[string]struct{}, len(oldData))
			for tagValue, names := range oldData {
				set := make(map[string]struct{}, len(names))
				for _, name := range names {
					set[name] = struct{}{}
				}
				idx.data[eventSentTagKey][tagValue] = set
			}
		}
	}

	mgr.indexes.Store(bucket, idx)
}
