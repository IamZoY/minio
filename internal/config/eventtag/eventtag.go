// Copyright (c) 2015-2023 MinIO, Inc.
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

package eventtag

import (
	"strings"
	"sync"

	"github.com/IamZoY/minio/internal/config"
	"github.com/minio/pkg/v3/env"
)

const (
	enableEventTagging = "enable_event_tagging"
	tagName            = "tag_name"
	tagSuccess         = "tag_success"
	tagFailed          = "tag_failed"
	eventTypes         = "event_types"

	envEventTagging = "MINIO_EVENT_TAG_ENABLE_EVENT_TAGGING"
	envTagName      = "MINIO_EVENT_TAG_TAG_NAME"
	envTagSuccess   = "MINIO_EVENT_TAG_TAG_SUCCESS"
	envTagFailed    = "MINIO_EVENT_TAG_TAG_FAILED"
	envEventTypes   = "MINIO_EVENT_TAG_EVENT_TYPES"

	defaultTagName    = "EventSent"
	defaultTagSuccess = "Success"
	defaultTagFailed  = "Failed"
	defaultEventTypes = "s3:ObjectCreated:Put,s3:ObjectCreated:Post,s3:ObjectCreated:Copy,s3:ObjectCreated:CompleteMultipartUpload"
)

// DefaultKVS - default event tag config
var DefaultKVS = config.KVS{
	config.KV{
		Key:   enableEventTagging,
		Value: config.EnableOff,
	},
	config.KV{
		Key:   tagName,
		Value: defaultTagName,
	},
	config.KV{
		Key:   tagSuccess,
		Value: defaultTagSuccess,
	},
	config.KV{
		Key:   tagFailed,
		Value: defaultTagFailed,
	},
	config.KV{
		Key:   eventTypes,
		Value: defaultEventTypes,
	},
}

// Config holds a single event tag rule.
type Config struct {
	EnableEventTagging bool     `json:"enable_event_tagging"`
	TagName            string   `json:"tag_name"`
	TagSuccess         string   `json:"tag_success"`
	TagFailed          string   `json:"tag_failed"`
	EventTypes         []string `json:"event_types"`
}

// MatchesEvent returns true if the given S3 event name is in EventTypes.
func (cfg Config) MatchesEvent(eventName string) bool {
	for _, et := range cfg.EventTypes {
		if et == eventName {
			return true
		}
	}
	return false
}

// MultiConfig holds all event tag target configs keyed by target name.
type MultiConfig struct {
	mu      sync.RWMutex
	targets map[string]Config
}

// NewMultiConfig creates an empty MultiConfig.
func NewMultiConfig() *MultiConfig {
	return &MultiConfig{targets: make(map[string]Config)}
}

// Update replaces all targets atomically.
func (mc *MultiConfig) Update(targets map[string]Config) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.targets = targets
}

// GetAll returns a snapshot of all enabled configs.
func (mc *MultiConfig) GetAll() []Config {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	result := make([]Config, 0, len(mc.targets))
	for _, cfg := range mc.targets {
		if cfg.EnableEventTagging {
			result = append(result, cfg)
		}
	}
	return result
}

// AnyEnabled returns true if at least one target is enabled.
func (mc *MultiConfig) AnyEnabled() bool {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	for _, cfg := range mc.targets {
		if cfg.EnableEventTagging {
			return true
		}
	}
	return false
}

// LookupConfig parses a single target's KVS into a Config.
func LookupConfig(kvs config.KVS) (cfg Config, err error) {
	cfg = Config{
		EnableEventTagging: false,
		TagName:            defaultTagName,
		TagSuccess:         defaultTagSuccess,
		TagFailed:          defaultTagFailed,
		EventTypes:         strings.Split(defaultEventTypes, ","),
	}

	if err = config.CheckValidKeys(config.EventTagSubSys, kvs, DefaultKVS); err != nil {
		return cfg, err
	}

	cfg.EnableEventTagging = env.Get(envEventTagging, kvs.GetWithDefault(enableEventTagging, DefaultKVS)) == config.EnableOn
	cfg.TagName = env.Get(envTagName, kvs.GetWithDefault(tagName, DefaultKVS))
	cfg.TagSuccess = env.Get(envTagSuccess, kvs.GetWithDefault(tagSuccess, DefaultKVS))
	cfg.TagFailed = env.Get(envTagFailed, kvs.GetWithDefault(tagFailed, DefaultKVS))

	etStr := env.Get(envEventTypes, kvs.GetWithDefault(eventTypes, DefaultKVS))
	if etStr != "" {
		cfg.EventTypes = strings.Split(etStr, ",")
	}

	return cfg, nil
}
