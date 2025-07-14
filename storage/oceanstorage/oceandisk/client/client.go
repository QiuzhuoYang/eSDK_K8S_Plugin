/*
 *  Copyright (c) Huawei Technologies Co., Ltd. 2024-2025. All rights reserved.
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

// Package client provides oceandisk storage client
package client

import (
	"context"

	"github.com/Huawei/eSDK_K8S_Plugin/v4/storage/oceanstorage/base"
)

// OceandiskClientInterface defines interfaces for base client operations
type OceandiskClientInterface interface {
	base.RestClientInterface
	base.ApplicationType
	base.FC
	base.Host
	base.Iscsi
	base.Mapping
	base.Qos
	base.RoCE
	base.System

	Namespace
	NamespaceGroup

	GetBackendID() string
	GetDeviceSN() string
	GetStorageVersion() string
}

// OceandiskClient implements OceandiskClientInterface
type OceandiskClient struct {
	*base.ApplicationTypeClient
	*base.FCClient
	*base.HostClient
	*base.IscsiClient
	*base.MappingClient
	*base.QosClient
	*base.RoCEClient
	*base.SystemClient

	*base.RestClient
}

// NewClient inits a new client of oceandisk client
func NewClient(ctx context.Context, param *base.NewClientConfig) (*OceandiskClient, error) {
	restClient, err := base.NewRestClient(ctx, param)
	if err != nil {
		return nil, err
	}

	return &OceandiskClient{
		ApplicationTypeClient: &base.ApplicationTypeClient{RestClientInterface: restClient},
		FCClient:              &base.FCClient{RestClientInterface: restClient},
		HostClient:            &base.HostClient{RestClientInterface: restClient},
		IscsiClient:           &base.IscsiClient{RestClientInterface: restClient},
		MappingClient:         &base.MappingClient{RestClientInterface: restClient},
		QosClient:             &base.QosClient{RestClientInterface: restClient},
		RoCEClient:            &base.RoCEClient{RestClientInterface: restClient},
		SystemClient:          &base.SystemClient{RestClientInterface: restClient},
		RestClient:            restClient,
	}, nil
}
