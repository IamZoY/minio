// Copyright (c) 2015-2023 MinIO, Inc.
//
// # This file is part of MinIO Object Storage stack
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

import "github.com/IamZoY/minio/internal/config"

// Help template for event tag feature.
var (
	defaultHelpPostfix = func(key string) string {
		return config.DefaultHelpPostfix(DefaultKVS, key)
	}

	Help = config.HelpKVS{
		config.HelpKV{
			Key:         enableEventTagging,
			Description: `turn 'on' to enable automatic tagging of objects based on event delivery status` + defaultHelpPostfix(enableEventTagging),
			Optional:    true,
			Type:        "on|off",
		},
		config.HelpKV{
			Key:         tagName,
			Description: `tag key applied to objects on event delivery` + defaultHelpPostfix(tagName),
			Optional:    true,
			Type:        "string",
		},
		config.HelpKV{
			Key:         tagSuccess,
			Description: `tag value when event is delivered successfully` + defaultHelpPostfix(tagSuccess),
			Optional:    true,
			Type:        "string",
		},
		config.HelpKV{
			Key:         tagFailed,
			Description: `tag value when event delivery fails` + defaultHelpPostfix(tagFailed),
			Optional:    true,
			Type:        "string",
		},
		config.HelpKV{
			Key:         eventTypes,
			Description: `comma-separated S3 event types that trigger this tag rule` + defaultHelpPostfix(eventTypes),
			Optional:    true,
			Type:        "string",
		},
	}
)
