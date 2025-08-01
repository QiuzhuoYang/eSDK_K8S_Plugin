/*
 *  Copyright (c) Huawei Technologies Co., Ltd. 2020-2025. All rights reserved.
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

// Package client provides oceanstor storage client
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync/atomic"

	"github.com/Huawei/eSDK_K8S_Plugin/v4/storage/oceanstorage/base"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/utils"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/utils/log"
)

const (
	// DefaultParallelCount defines default parallel count
	DefaultParallelCount int = 30

	// MaxParallelCount defines max parallel count
	MaxParallelCount int = 30

	// MinParallelCount defines min parallel count
	MinParallelCount int = 1

	// UrlNotFound defines error msg of url not found
	UrlNotFound = "404_NotFound"
)

const (
	description string = "Created from huawei-csi for Kubernetes"
)

// OceanstorClientInterface defines interfaces for base client operations
type OceanstorClientInterface interface {
	base.RestClientInterface
	base.ApplicationType
	base.FC
	base.Host
	base.Iscsi
	base.Mapping
	base.Qos
	base.RoCE
	base.System

	Clone
	OceanstorFilesystem
	FSSnapshot
	HyperMetro
	Lun
	LunCopy
	LunSnapshot
	Replication
	OceanstorVStore
	DTree
	OceanStorQuota
	LIF

	SafeCall(ctx context.Context, method string, url string, data map[string]interface{}) (base.Response, error)
	SafeBaseCall(ctx context.Context, method string, url string, data map[string]interface{}) (base.Response, error)
	SafeDelete(ctx context.Context, url string, data map[string]interface{}) (base.Response, error)
	DuplicateClient() *OceanstorClient

	GetBackendID() string
	GetDeviceSN() string
	GetStorageVersion() string
	GetCurrentSiteWwn() string
	SetSystemInfo(ctx context.Context) error
}

var (
	filterLog = map[string]map[string]bool{
		"POST": {
			"/xx/sessions": true,
		},
	}

	filterLogRegex = map[string][]string{
		"GET": {
			`/vstore_pair\?filter=ID`,
			`/FsHyperMetroDomain\?RUNNINGSTATUS=0`,
			`/remote_device`,
		},
	}

	debugLog = map[string]map[string]bool{
		"GET": {
			"/license/feature":        true,
			"/nfsservice":             true,
			"/storagepool":            true,
			`/vstore_pair\?REPTYPE=1`: true,
			`/vstore?filter=NAME`:     true,
			`/container_pv`:           true,
			`/system`:                 true,
		},
	}

	debugLogRegex = map[string][]string{
		"GET": {
			`/vstore_pair\?REPTYPE=1`,
			`/vstore\?filter=NAME`,
			`/system`,
		},
	}
)

func isFilterLog(method, url string) bool {
	if filter, exist := filterLog[method]; exist && filter[url] {
		return true
	}

	if filter, exist := filterLogRegex[method]; exist {
		for _, k := range filter {
			match, err := regexp.MatchString(k, url)
			if err == nil && match {
				return true
			}
		}
	}

	return false
}

// OceanstorClient implements OceanstorClientInterface
type OceanstorClient struct {
	*base.ApplicationTypeClient
	*base.FCClient
	*base.HostClient
	*base.IscsiClient
	*base.MappingClient
	*base.QosClient
	*base.RoCEClient
	*base.SystemClient
	*base.FilesystemClient
	*base.VStoreClient

	*RestClient
}

// NewClientConfig stores the information needed to create a new oceanstor client
type NewClientConfig struct {
	Urls               []string
	User               string
	SecretName         string
	SecretNamespace    string
	VstoreName         string
	ParallelNum        string
	BackendID          string
	UseCert            bool
	CertSecretMeta     string
	Storage            string
	Name               string
	AuthenticationMode string
}

// NewClient inits a new oceanstor client
func NewClient(ctx context.Context, param *NewClientConfig) (*OceanstorClient, error) {
	restClient, err := NewRestClient(ctx, param)
	if err != nil {
		return nil, err
	}

	return &OceanstorClient{
		ApplicationTypeClient: &base.ApplicationTypeClient{RestClientInterface: restClient},
		FCClient:              &base.FCClient{RestClientInterface: restClient},
		HostClient:            &base.HostClient{RestClientInterface: restClient},
		IscsiClient:           &base.IscsiClient{RestClientInterface: restClient},
		MappingClient:         &base.MappingClient{RestClientInterface: restClient},
		QosClient:             &base.QosClient{RestClientInterface: restClient},
		RoCEClient:            &base.RoCEClient{RestClientInterface: restClient},
		SystemClient:          &base.SystemClient{RestClientInterface: restClient},
		FilesystemClient:      &base.FilesystemClient{RestClientInterface: restClient},
		VStoreClient:          &base.VStoreClient{RestClientInterface: restClient},
		RestClient:            restClient,
	}, nil
}

// SafeCall provides call for restful request
func (cli *OceanstorClient) SafeCall(ctx context.Context,
	method string, url string,
	data map[string]interface{}) (base.Response, error) {
	var r base.Response
	var err error

	r, err = cli.SafeBaseCall(ctx, method, url, data)
	if !base.NeedReLogin(r, err) {
		return r, err
	}

	// Current connection fails, try to relogin to other Urls if exist,
	// if relogin success, resend the request again.
	log.AddContext(ctx).Infof("Try to re-login and resend request method: %s, Url: %s", method, url)
	err = cli.ReLogin(ctx)
	if err != nil {
		return r, err
	}

	// If the logical port changes from storage A to storage B, the system information must be updated.
	if err = cli.SetSystemInfo(ctx); err != nil {
		log.AddContext(ctx).Errorf("after re-login, can't get system info, error: %v", err)
		return r, err
	}

	return cli.SafeBaseCall(ctx, method, url, data)
}

// SafeBaseCall provides base call for request
func (cli *OceanstorClient) SafeBaseCall(ctx context.Context,
	method string,
	url string,
	data map[string]interface{}) (base.Response, error) {
	var req *http.Request
	var err error

	if cli.Client == nil {
		return base.Response{}, fmt.Errorf("failed to send request method: %s, url: %s,"+
			" cause by client not init", method, url)
	}

	reqUrl := cli.Url
	reqUrl += url

	if url != "/xx/sessions" && url != "/sessions" {
		cli.ReLoginMutex.Lock()
		req, err = cli.GetRequest(ctx, method, url, data)
		cli.ReLoginMutex.Unlock()
	} else {
		req, err = cli.GetRequest(ctx, method, url, data)
	}

	if err != nil || req == nil {
		return base.Response{}, fmt.Errorf("get request failed, error: %w", err)
	}

	log.FilteredLog(ctx, isFilterLog(method, url), utils.IsDebugLog(method, url, debugLog, debugLogRegex),
		fmt.Sprintf("Request method: %s, Url: %s, body: %v", method, req.URL, data))

	if cli.RequestSemaphore != nil {
		cli.RequestSemaphore.Acquire()
		defer cli.RequestSemaphore.Release()
	}

	if base.RequestSemaphoreMap[cli.GetDeviceSN()] != nil {
		base.RequestSemaphoreMap[cli.GetDeviceSN()].Acquire()
		defer base.RequestSemaphoreMap[cli.GetDeviceSN()].Release()
	}

	return cli.safeDoCall(ctx, method, url, req)
}

func (cli *OceanstorClient) safeDoCall(ctx context.Context,
	method string, url string, req *http.Request) (base.Response, error) {
	// check whether the logical port is changed from A to B before invoking.
	// The possible cause is that other invoking operations are performed for re-login.
	isNotSessionUrl := url != "/xx/sessions" && url != "/sessions"
	if isNotSessionUrl && cli.CurrentLifWwn != "" {
		if cli.systemInfoRefreshing() {
			return base.Response{}, errors.New("querying lif and system information... Please wait")
		}

		if cli.CurrentLifWwn != cli.CurrentSiteWwn {
			currentPort := cli.GetCurrentLif(ctx)
			log.AddContext(ctx).Errorf("current logical port [%s] is not running on own site, "+
				"currentLifWwn: %s, currentSiteWwn: %s", currentPort, cli.CurrentLifWwn, cli.CurrentSiteWwn)
			return base.Response{}, fmt.Errorf("current logical port [%s] is not running on own site", currentPort)
		}
	}

	resp, err := cli.Client.Do(req)
	if err != nil {
		log.AddContext(ctx).Errorf("Send request method: %s, Url: %s, error: %v", method, req.URL, err)
		return base.Response{}, errors.New(base.Unconnected)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.AddContext(ctx).Infof("read close resp failed, error %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return base.Response{}, fmt.Errorf("read response data error: %w", err)
	}

	log.FilteredLog(ctx, isFilterLog(method, url), utils.IsDebugLog(method, url, debugLog, debugLogRegex),
		fmt.Sprintf("base.Response method: %s, Url: %s, body: %s", method, req.URL, body))

	var r base.Response
	err = json.Unmarshal(body, &r)
	if err != nil {
		return base.Response{}, fmt.Errorf("json.Unmarshal data %s error: %w", body, err)
	}

	return r, nil
}

// SafeDelete provides http request of DELETE method
func (cli *OceanstorClient) SafeDelete(ctx context.Context,
	url string, data map[string]interface{}) (base.Response, error) {
	return cli.SafeCall(ctx, "DELETE", url, data)
}

// DuplicateClient clone a base client from origin client
func (cli *OceanstorClient) DuplicateClient() *OceanstorClient {
	dup := *cli

	dup.Urls = make([]string, len(cli.Urls))
	copy(dup.Urls, cli.Urls)

	dup.Client = nil

	return &dup
}

func (cli *OceanstorClient) getResponseDataMap(ctx context.Context, data interface{}) (map[string]interface{}, error) {
	respData, ok := data.(map[string]interface{})
	if !ok {
		return nil, utils.Errorf(ctx, "the response data is not a map[string]interface{}")
	}

	if len(respData) == 0 {
		return nil, nil
	}

	return respData, nil
}

func (cli *OceanstorClient) getResponseDataList(ctx context.Context, data interface{}) ([]interface{}, error) {
	respData, ok := data.([]interface{})
	if !ok {
		return nil, utils.Errorf(ctx, "the response data is not a []interface{}")
	}
	return respData, nil
}

func (cli *OceanstorClient) getObjByvStoreName(objList []interface{}) map[string]interface{} {
	for _, data := range objList {
		obj, ok := data.(map[string]interface{})
		if !ok || obj == nil {
			continue
		}

		vStoreName, ok := obj["vstoreName"].(string)
		if !ok {
			vStoreName = base.DefaultVStore
		}

		if vStoreName == cli.GetvStoreName() {
			return obj
		}
		continue

	}
	return nil
}

func (cli *OceanstorClient) systemInfoRefreshing() bool {
	return atomic.LoadUint32(&cli.SystemInfoRefreshing) == 1
}
