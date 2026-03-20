# Event Tag Index: Scalable Object Lookup by Tag

## Overview

MinIO's Event Tag Index allows querying objects by their tags (e.g., "give me all objects where `EventSent=Failed`") without scanning the entire bucket. This is used primarily by the event notification system to track which objects have been successfully delivered to notification targets.

When event tagging is enabled (`MINIO_EVENT_TAG_ENABLE_EVENT_TAGGING=on`), objects are automatically tagged with `EventSent=Success` or `EventSent=Failed` after event delivery. The tag index provides fast lookups against these tags.

---

## Approach Comparison

### Approach 1: ListObjects + Filter (Naive)

The simplest way to find objects with a specific tag is to list all objects, read each object's tags, and filter:

```
ListObjectsV2(bucket) → for each object → GetObjectTags(object) → filter by tag
```

| Metric | 1K objects | 100K objects | 1M objects | 10M objects |
|--------|-----------|-------------|-----------|------------|
| API calls | 1,001 | 100,001 | 1,000,001 | 10,000,001 |
| Disk reads | ~1,001 | ~100,001 | ~1,000,001 | ~10,000,001 |
| Memory | ~1 MB | ~100 MB | ~1 GB | ~10 GB |
| Latency | ~2s | ~3 min | ~30 min | ~5+ hours |

**Verdict**: Unusable at scale. Every query re-scans the entire bucket.

---

### Approach 2: Single In-Memory Map + JSON File (Previous Implementation)

The previous implementation kept all object names in a Go map in memory:

```go
data map[string]map[string]map[string]struct{}
// tagKey -> tagValue -> set of objectNames
```

Flushed every 5 seconds to a single JSON file:
```
.minio.sys/event-tag-index/{bucket}.json   ← one file, all names
```

| Metric | 1K objects | 100K objects | 1M objects | 10M objects |
|--------|-----------|-------------|-----------|------------|
| RAM (constant) | ~1 MB | ~15 MB | ~200 MB | ~2 GB |
| Flush write size | ~20 KB | ~2 MB | ~50 MB | ~500 MB |
| Flush frequency | every 5s | every 5s | every 5s | every 5s |
| Startup load time | instant | ~100ms | ~5s | ~30-60s (blocks boot) |
| Query latency | <1ms | <1ms | <1ms | <1ms (if RAM available) |
| Update channel cap | 10,000 | 10,000 | 10,000 | 10,000 (drops updates) |

**Verdict**: Fast queries but doesn't scale. At 10M objects, consumes 2GB RAM permanently, writes 500MB to disk every 5 seconds, and blocks MinIO startup for up to a minute while parsing the JSON. The fixed 10K update channel drops events under load.

---

### Approach 3: Sharded Inverted Index with Delta Buffering (Current Implementation)

The current implementation stores sorted object names in compressed chunk files on disk, keeps only lightweight metadata in memory, and buffers updates in small deltas:

```
.minio.sys/event-tag-index/{bucket}/
  _meta.json.zst                          ← counts + chunk boundaries (~1-20 KB)
  {tagKey}/{tagValue}/
    chunk-000000.json.zst                 ← sorted names, max 50K per chunk (~500 KB)
    chunk-000001.json.zst
    ...
```

| Metric | 1K objects | 100K objects | 1M objects | 10M objects |
|--------|-----------|-------------|-----------|------------|
| RAM (constant) | ~20 KB | ~25 KB | ~30 KB | ~50 KB |
| RAM (peak, during query) | ~50 KB | ~1 MB | ~2 MB | ~2 MB |
| Disk write per update batch | ~10 KB | ~50 KB | ~100 KB | ~100 KB |
| Startup load time | instant | instant | instant | instant |
| Query latency (1 page) | ~1ms | ~2ms | ~3ms | ~3ms |
| Stream all (1 request) | ~5ms | ~50ms | ~500ms | ~5s |
| Update channel cap | 100,000 | 100,000 | 100,000 | 100,000 |
| Update workers | 4 | 4 | 4 | 4 |
| .minio.sys usage | ~1 KB | ~1 KB | ~1 KB | ~1 KB |
| Index in bucket | ~5 KB | ~500 KB | ~15 MB | ~150 MB |

**How it works:**

1. **In-memory**: Only tag counts and chunk boundary markers (~50 KB at 10M objects)
2. **On disk**: Sorted chunks of 50,000 object names each, newline-delimited text compressed with zstd, stored in the user's bucket under `.minio.tag-index/`
3. **Meta in .minio.sys**: Only a ~1 KB meta file per bucket (counts + chunk boundaries)
4. **Updates**: Buffered in memory, flushed to disk and compacted when buffer hits 5,000 entries
5. **Queries**: Binary search chunk boundaries → read 1-2 chunks → merge pending delta → return page
6. **Startup**: Loads only the tiny meta file per bucket — MinIO boots instantly

---

## API Endpoints

### Paginated Query

```
GET /{bucket}/?list-by-tag&tag-key=EventSent&tag-value=Failed&marker=&max-keys=1000
```

Returns a JSON page of matching objects:

```json
{
  "objects": [
    {"key": "photo1.jpg", "tagValue": "Failed"},
    {"key": "document.pdf", "tagValue": "Failed"}
  ],
  "isTruncated": true,
  "nextMarker": "document.pdf",
  "totalMatchCount": 2000000
}
```

Use `nextMarker` from the response as `marker` in the next request to paginate.

**Use when**: You need a specific page, or are building a UI with pagination.

### Streaming Query (All Results)

```
GET /{bucket}/?stream-by-tag&tag-key=EventSent&tag-value=Failed
```

Returns ALL matching objects as newline-delimited JSON (NDJSON), streamed:

```jsonl
{"key":"aaa-first.jpg","tagValue":"Failed"}
{"key":"aab-second.jpg","tagValue":"Failed"}
... (millions of lines streamed)
{"totalCount":2000000,"done":true}
```

The response streams as chunks are read from disk — memory usage stays at ~2 MB regardless of result size. The last line contains `"done": true` with the total count.

**Use when**: You need all matching objects in one request (e.g., for retry pipelines, auditing, bulk operations).

### Admin: Rebuild Index

```
POST /minio/admin/v3/rebuild-tag-index?bucket={bucket}
```

Scans all objects in the bucket and rebuilds the index from scratch. Use after:
- Enabling event tagging on a bucket that already has tagged objects
- Suspecting index drift (e.g., after a crash during heavy writes)

Returns:
```json
{
  "status": "success",
  "bucket": "my-bucket",
  "counts": {
    "EventSent": {
      "Success": 7500000,
      "Failed": 2000000,
      "Untagged": 500000
    }
  }
}
```

---

## Architecture

### Storage Layout

```
.minio.sys/
  event-tag-index/{bucket}/
    _meta.json.zst                          ← metadata only (~1 KB): counts, chunk bounds

{bucket}/
  .minio.tag-index/
    EventSent/
      Success/
        chunk-000000.txt.zst                ← sorted object names, text+zstd (~250 KB)
        chunk-000001.txt.zst
        ...
      Failed/
        chunk-000000.txt.zst
        ...
      Untagged/
        chunk-000000.txt.zst
        ...
```

### Update Flow

```
Object created → Event sent → applyEventTagging()
  → PutObjectTags("EventSent", "Success")
  → SendIndexUpdate(bucket, object, "EventSent", "Success")
  → updateCh (buffered channel, cap 100K)
  → 4 update workers drain channel
  → apply to in-memory delta buffer
  → when delta reaches 5K entries → compaction worker
    → read affected chunks from user bucket (.minio.tag-index/)
    → merge adds/removes into sorted chunks
    → write back text+zstd chunks to user bucket
    → update meta in .minio.sys
```

### Query Flow (Paginated)

```
GET /?list-by-tag&tag-key=EventSent&tag-value=Failed&marker=photo.jpg&max-keys=100
  → load bucketTagMeta from sync.Map (in-memory, ~1 KB)
  → binary search ChunkBounds to find starting chunk index
  → read 1-2 chunk files from user bucket (~250 KB each, text+zstd)
  → merge with pending in-memory delta (adds/removes)
  → return sorted page of up to max-keys results
```

### Query Flow (Streaming)

```
GET /?stream-by-tag&tag-key=EventSent&tag-value=Failed
  → set response headers (NDJSON, chunked transfer)
  → for each chunk file (0..N):
    → read chunk from user bucket (~250 KB, text+zstd)
    → filter out pending delta removes
    → write each name as JSON line to response
    → flush to client
  → write delta adds
  → write final summary line {"totalCount": N, "done": true}
```

---

## Migration

On first startup after upgrading, the system automatically migrates through all historical formats:

**v0 (oldest)**: Single `event-tag-index/{bucket}.json` file in `.minio.sys`
**v1**: Sharded JSON chunks in `.minio.sys/event-tag-index/{bucket}/{tagKey}/{tagValue}/chunk-*.json.zst`
**v2 (current)**: Text+zstd chunks in `{bucket}/.minio.tag-index/{tagKey}/{tagValue}/chunk-*.txt.zst`, meta only in `.minio.sys`

Migration runs in background on startup, one bucket at a time:
1. Detects the format version from the meta file
2. Reads old chunks, re-encodes as newline-delimited text+zstd
3. Writes to the user's bucket under `.minio.tag-index/`
4. Deletes old files from `.minio.sys`
5. Updates meta with `format: "v2"`

No manual intervention required.

---

## Configuration

Event tagging is controlled by:

```
MINIO_EVENT_TAG_ENABLE_EVENT_TAGGING=on   # Enable automatic event tagging
```

Or via the MinIO configuration subsystem under the `event_tag` key.

The tag index is automatically managed when event tagging is enabled. No additional configuration is needed for the index itself.
