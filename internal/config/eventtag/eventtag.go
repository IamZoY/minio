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
	"sync"

	"github.com/IamZoY/minio/internal/config"
	"github.com/minio/pkg/v3/env"
)

// Event tag sub-system constants
const (
	// enableEventTagging setting name for enabling event tagging
	enableEventTagging = "enable_event_tagging"

	EnvEventTagging = "MINIO_EVENT_TAG_ENABLE_EVENT_TAGGING"
)

// DefaultKVS - default event tag config
var (
	DefaultKVS = config.KVS{
		config.KV{
			Key:   enableEventTagging,
			Value: config.EnableOff,
		},
	}
)

// configLock is a global lock for event tag config
var configLock sync.RWMutex

// Config holds event tagging configuration
type Config struct {
	EnableEventTagging bool `json:"enable_event_tagging"`
}

// Update updates event tag config with new config
func (cfg *Config) Update(newCfg Config) {
	configLock.Lock()
	defer configLock.Unlock()
	cfg.EnableEventTagging = newCfg.EnableEventTagging
}

// LookupConfig - lookup event tag config and override with valid environment settings if any.
func LookupConfig(kvs config.KVS) (cfg Config, err error) {
	cfg = Config{
		EnableEventTagging: false,
	}

	if err = config.CheckValidKeys(config.EventTagSubSys, kvs, DefaultKVS); err != nil {
		return cfg, err
	}

	enableEventTagging := env.Get(EnvEventTagging, kvs.GetWithDefault(enableEventTagging, DefaultKVS)) == config.EnableOn
	cfg.EnableEventTagging = enableEventTagging

	return cfg, nil
}

// IsEnabled - returns whether event tagging is enabled
func (cfg *Config) IsEnabled() bool {
	configLock.RLock()
	defer configLock.RUnlock()
	return cfg.EnableEventTagging
}
