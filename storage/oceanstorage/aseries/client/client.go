/*
 *  Copyright (c) Huawei Technologies Co., Ltd. 2025-2025. All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

// Package client provides oceanstor A-series storage client
package client

import (
	"context"

	"github.com/Huawei/eSDK_K8S_Plugin/v4/storage/oceanstorage/base"
)

// OceanASeriesClientInterface defines interfaces for A-series client operations
type OceanASeriesClientInterface interface {
	base.RestClientInterface
	base.ApplicationType
	base.Qos
	base.System

	ASeriesVStore
	ASeriesFilesystem

	GetBackendID() string
	GetDeviceSN() string
	GetStorageVersion() string
}

// OceanASeriesClient implements OceanASeriesClientInterface
type OceanASeriesClient struct {
	*base.ApplicationTypeClient
	*base.QosClient
	*base.SystemClient
	*base.VStoreClient
	*base.FilesystemClient
	*base.RestClient
}

// NewClient inits a new client of oceanstor A-series client
func NewClient(ctx context.Context, param *base.NewClientConfig) (*OceanASeriesClient, error) {
	restClient, err := base.NewRestClient(ctx, param)
	if err != nil {
		return nil, err
	}

	return &OceanASeriesClient{
		ApplicationTypeClient: &base.ApplicationTypeClient{RestClientInterface: restClient},
		QosClient:             &base.QosClient{RestClientInterface: restClient},
		SystemClient:          &base.SystemClient{RestClientInterface: restClient},
		VStoreClient:          &base.VStoreClient{RestClientInterface: restClient},
		FilesystemClient:      &base.FilesystemClient{RestClientInterface: restClient},
		RestClient:            restClient,
	}, nil
}
