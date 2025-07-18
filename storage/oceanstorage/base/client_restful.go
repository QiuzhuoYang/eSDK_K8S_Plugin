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

// Package base provide base operations for oceanstor base storage
package base

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	pkgUtils "github.com/Huawei/eSDK_K8S_Plugin/v4/pkg/utils"
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
)

var (
	filterLog = map[string]map[string]bool{
		"POST": {
			"/xx/sessions": true,
		},
	}
	debugLog = map[string]map[string]bool{
		"GET": {
			"/license/feature":    true,
			"/nfsservice":         true,
			`/vstore?filter=NAME`: true,
			"/storagepool":        true,
			`/system`:             true,
		},
	}

	debugLogRegex = map[string][]string{
		"GET": {
			`/vstore\?filter=NAME`,
			`/system`,
		},
	}
)

func isFilterLog(method, url string) bool {
	if filter, exist := filterLog[method]; exist && filter[url] {
		return true
	}

	return false
}

// RestClientInterface defines interfaces for base restful call
type RestClientInterface interface {
	Call(ctx context.Context, method string, url string, data map[string]interface{}) (Response, error)
	BaseCall(ctx context.Context, method string, url string, data map[string]interface{}) (Response, error)
	Get(ctx context.Context, url string, data map[string]interface{}) (Response, error)
	Post(ctx context.Context, url string, data map[string]interface{}) (Response, error)
	Put(ctx context.Context, url string, data map[string]interface{}) (Response, error)
	Delete(ctx context.Context, url string, data map[string]interface{}) (Response, error)
	GetRequest(ctx context.Context, method string, url string, data map[string]interface{}) (*http.Request, error)
	Login(ctx context.Context) error
	Logout(ctx context.Context)
	ReLogin(ctx context.Context) error
	GetSystem(ctx context.Context) (map[string]interface{}, error)
	ValidateLogin(ctx context.Context) error
}

// RestClient defines client implements the rest interface
type RestClient struct {
	Client HTTP
	Url    string
	Urls   []string

	VStoreID        string
	VStoreName      string
	User            string
	SecretNamespace string
	SecretName      string
	StorageVersion  string
	BackendID       string
	Storage         string
	DeviceId        string
	Token           string

	SystemInfoRefreshing uint32
	ReLoginMutex         sync.Mutex
	RequestSemaphore     *utils.Semaphore
}

// NewClientConfig stores the information needed to create a new rest client
type NewClientConfig struct {
	Urls            []string
	User            string
	SecretName      string
	SecretNamespace string
	ParallelNum     string
	BackendID       string
	UseCert         bool
	CertSecretMeta  string
	Storage         string
	Name            string
}

// NewRestClient inits a new rest client
func NewRestClient(ctx context.Context, param *NewClientConfig) (*RestClient, error) {
	var err error
	var parallelCount int

	parallelCount, err = strconv.Atoi(param.ParallelNum)
	if err != nil || parallelCount > MaxParallelCount || parallelCount < MinParallelCount {
		log.Warningf("the config parallelNum %d is invalid, set it to the default value %d",
			parallelCount, DefaultParallelCount)
		parallelCount = DefaultParallelCount
	}

	log.AddContext(ctx).Infof("Init parallel count is %d", parallelCount)
	httpClient, err := NewHTTPClientByCertMeta(ctx, param.UseCert, param.CertSecretMeta)
	if err != nil {
		log.AddContext(ctx).Errorf("new http client by cert meta failed, err is %v", err)
		return nil, err
	}

	return &RestClient{
		Urls:             param.Urls,
		User:             param.User,
		Storage:          param.Storage,
		SecretName:       param.SecretName,
		SecretNamespace:  param.SecretNamespace,
		Client:           httpClient,
		BackendID:        param.BackendID,
		RequestSemaphore: utils.NewSemaphore(parallelCount),
	}, nil
}

// Call provides call for restful request
func (cli *RestClient) Call(ctx context.Context,
	method string, url string, data map[string]interface{}) (Response, error) {
	var r Response
	var err error

	r, err = cli.BaseCall(ctx, method, url, data)
	if !NeedReLogin(r, err) {
		return r, err
	}

	// Current connection fails, try to relogin to other Urls if exist,
	// if relogin success, resend the request again.
	log.AddContext(ctx).Infof("try to relogin and resend request method: %s, Url: %s", method, url)
	err = cli.ReLogin(ctx)
	if err != nil {
		return Response{}, err
	}

	if err = cli.SetSystemInfo(ctx); err != nil {
		return Response{}, fmt.Errorf("after relogin, can't get system info, error: %v", err)
	}

	return cli.BaseCall(ctx, method, url, data)
}

// SetSystemInfo set system info
// the mutex lock is required for re-login. Therefore, the internal query of the login interface cannot be performed.
func (cli *RestClient) SetSystemInfo(ctx context.Context) error {
	log.AddContext(ctx).Infof("set backend [%s] system info is refreshing", cli.BackendID)
	atomic.StoreUint32(&cli.SystemInfoRefreshing, 1)
	defer func() {
		log.AddContext(ctx).Infof("set backend [%s] system info are refreshed", cli.BackendID)
		atomic.StoreUint32(&cli.SystemInfoRefreshing, 0)
	}()

	system, err := cli.GetSystem(ctx)
	if err != nil {
		return err
	}

	storagePointVersion, ok := utils.GetValue[string](system, "pointRelease")
	if ok {
		cli.StorageVersion = storagePointVersion
	}

	log.AddContext(ctx).Infof("backend type [%s], backend [%s], storage version [%s]",
		cli.Storage, cli.BackendID, cli.StorageVersion)
	return nil
}

// BaseCall provides base call for request
func (cli *RestClient) BaseCall(ctx context.Context,
	method string, url string, data map[string]interface{}) (Response, error) {
	var r Response
	var req *http.Request
	var err error

	if cli.Client == nil {
		errMsg := "http client is nil"
		log.AddContext(ctx).Errorf("Failed to send request method: %s, url: %s, error: %s", method, url, errMsg)
		return Response{}, errors.New(errMsg)
	}

	if url != "/xx/sessions" && url != "/sessions" {
		cli.ReLoginMutex.Lock()
		req, err = cli.GetRequest(ctx, method, url, data)
		cli.ReLoginMutex.Unlock()
	} else {
		req, err = cli.GetRequest(ctx, method, url, data)
	}

	if err != nil {
		return Response{}, err
	}

	log.FilteredLog(ctx, isFilterLog(method, url), utils.IsDebugLog(method, url, debugLog, debugLogRegex),
		fmt.Sprintf("Request method: %s, Url: %s, body: %v", method, req.URL, data))

	if cli.RequestSemaphore == nil {
		return Response{}, errors.New("request semaphore is nil")
	}

	cli.RequestSemaphore.Acquire()
	defer cli.RequestSemaphore.Release()

	if RequestSemaphoreMap[cli.GetDeviceSN()] != nil {
		RequestSemaphoreMap[cli.GetDeviceSN()].Acquire()
		defer RequestSemaphoreMap[cli.GetDeviceSN()].Release()
	} else {
		RequestSemaphoreMap[UninitializedStorage].Acquire()
		defer RequestSemaphoreMap[UninitializedStorage].Release()
	}

	resp, err := cli.Client.Do(req)
	if err != nil {
		log.AddContext(ctx).Errorf("Send request method: %s, Url: %s, error: %v", method, req.URL, err)
		return Response{}, errors.New(Unconnected)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.AddContext(ctx).Errorf("Read response data error: %v", err)
		return Response{}, err
	}

	log.FilteredLog(ctx, isFilterLog(method, url), utils.IsDebugLog(method, url, debugLog, debugLogRegex),
		fmt.Sprintf("Response method: %s, Url: %s, body: %s", method, req.URL, body))

	err = json.Unmarshal(body, &r)
	if err != nil {
		log.AddContext(ctx).Errorf("json.Unmarshal data %s error: %v", body, err)
		return Response{}, err
	}

	return r, nil
}

// Get provides http request of GET method
func (cli *RestClient) Get(ctx context.Context, url string, data map[string]interface{}) (Response, error) {
	return cli.Call(ctx, "GET", url, data)
}

// Post provides http request of POST method
func (cli *RestClient) Post(ctx context.Context, url string, data map[string]interface{}) (Response, error) {
	return cli.Call(ctx, "POST", url, data)
}

// Put provides http request of PUT method
func (cli *RestClient) Put(ctx context.Context, url string, data map[string]interface{}) (Response, error) {
	return cli.Call(ctx, "PUT", url, data)
}

// Delete provides http request of DELETE method
func (cli *RestClient) Delete(ctx context.Context, url string, data map[string]interface{}) (Response, error) {
	return cli.Call(ctx, "DELETE", url, data)
}

// GetRequest return the request info
func (cli *RestClient) GetRequest(ctx context.Context,
	method string, url string, data map[string]interface{}) (*http.Request, error) {
	var req *http.Request
	var err error

	reqUrl := cli.Url
	if cli.DeviceId != "" {
		reqUrl += "/" + cli.DeviceId
	}
	reqUrl += url

	var reqBody io.Reader

	if data != nil {
		reqBytes, err := json.Marshal(data)
		if err != nil {
			log.AddContext(ctx).Errorf("json.Marshal data %v error: %v", MaskRequestData(data), err)
			return nil, err
		}
		reqBody = bytes.NewReader(reqBytes)
	}

	req, err = http.NewRequest(method, reqUrl, reqBody)
	if err != nil {
		log.AddContext(ctx).Errorf("construct http request error: %v", err)
		return nil, err
	}

	if req == nil || req.Header == nil {
		log.AddContext(ctx).Errorln("construct http request error: request header init failed")
		return nil, err
	}

	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Type", "application/json")

	if cli.Token != "" {
		req.Header.Set("iBaseToken", cli.Token)
	}

	return req, nil
}

// Login login and set data from response
func (cli *RestClient) Login(ctx context.Context) error {
	var err error

	if cli.Client, err = NewHTTPClientByBackendID(ctx, cli.BackendID); err != nil {
		return pkgUtils.Errorln(ctx,
			fmt.Sprintf("new http client by backend %s failed, err is %v", cli.BackendID, err))
	}

	data, err := cli.getRequestParams(ctx, cli.BackendID)
	if err != nil {
		return pkgUtils.Errorln(ctx, fmt.Sprintf("get reuqest failed while login, error : %v", err))
	}

	cli.DeviceId, cli.Token = "", ""

	resp, err := cli.loginCall(ctx, data)
	if err != nil {
		return pkgUtils.Errorln(ctx, fmt.Sprintf("request storage failed while login, error : %v", err))
	}

	code, _, err := utils.FormatRespErr(resp.Error)
	if code != 0 {
		msg := fmt.Sprintf("login %s error: %+v", cli.Url, resp)
		if utils.Contains(WrongPasswordErrorCodes, code) || utils.Contains(AccountBeenLocked, code) ||
			code == IPLockErrorCode {
			if err := pkgUtils.SetStorageBackendContentOnlineStatus(ctx, cli.BackendID, false); err != nil {
				msg = msg + fmt.Sprintf("\nsetStorageBackendContentOffline [%s] failed, "+
					"error: %v", cli.BackendID, err)
			}
		}
		return pkgUtils.Errorln(ctx, msg)
	}

	if err = cli.setDataFromRespData(ctx, resp); err != nil {
		cli.Logout(ctx)
		setErr := pkgUtils.SetStorageBackendContentOnlineStatus(ctx, cli.BackendID, false)
		if setErr != nil {
			log.AddContext(ctx).Errorf("setStorageBackendContentOffline [%s] failed, "+
				"error: %v", cli.BackendID, setErr)
		}
		return pkgUtils.Errorln(ctx, err.Error())
	}
	return nil
}

func (cli *RestClient) loginCall(ctx context.Context, data map[string]interface{}) (Response, error) {
	var resp Response
	var err error
	for i, url := range cli.Urls {
		cli.Url = url + "/deviceManager/rest"
		log.AddContext(ctx).Infof("try to login %s", cli.Url)

		resp, err = cli.BaseCall(ctx, "POST", "/xx/sessions", data)
		if err == nil {
			/* Sort the login Url to the last slot of san addresses, so that
			   if this connection error, next time will try other Url first. */
			cli.Urls[i], cli.Urls[len(cli.Urls)-1] = cli.Urls[len(cli.Urls)-1], cli.Urls[i]
			break
		} else if err.Error() != Unconnected {
			log.AddContext(ctx).Errorf("login %s error", cli.Url)
			break
		}

		log.AddContext(ctx).Warningf("login %s error due to connection failure, gonna try another Url", cli.Url)
	}

	if err != nil {
		return Response{}, err
	}
	return resp, err
}

func (cli *RestClient) getRequestParams(ctx context.Context, backendID string) (map[string]interface{}, error) {
	authInfo, err := pkgUtils.GetAuthInfoFromBackendID(ctx, backendID)
	if err != nil {
		return nil, err
	}
	cli.User = authInfo.User

	data := map[string]interface{}{
		"username": authInfo.User,
		"password": authInfo.Password,
		"scope":    authInfo.Scope,
	}
	authInfo.Password = ""

	if len(cli.VStoreName) > 0 && cli.VStoreName != DefaultVStore {
		data["vstorename"] = cli.VStoreName
	}

	return data, err
}

func (cli *RestClient) setDataFromRespData(ctx context.Context, resp Response) error {
	respData, ok := resp.Data.(map[string]interface{})
	if !ok {
		return fmt.Errorf("convert resp.Data to map[string]interface{} failed, data type: [%T]", resp.Data)
	}

	cli.DeviceId, ok = utils.GetValue[string](respData, "deviceid")
	if !ok {
		return fmt.Errorf("convert respData[\"deviceid\"]: [%T] to string failed", respData["deviceid"])
	}

	if RequestSemaphoreMap[cli.DeviceId] == nil {
		RequestSemaphoreMap[cli.DeviceId] = utils.NewSemaphore(MaxStorageThreads)
	}

	cli.Token, ok = utils.GetValue[string](respData, "iBaseToken")
	if !ok {
		return fmt.Errorf("convert respData[\"iBaseToken\"]: [%T] to string failed", respData["iBaseToken"])
	}

	cli.VStoreName, ok = utils.GetValue[string](respData, "vstoreName")
	if !ok {
		log.AddContext(ctx).Infof("can not get vstoreName from response, set it to default %s", DefaultVStore)
		cli.VStoreName = DefaultVStore
	}

	cli.VStoreID, ok = utils.GetValue[string](respData, "vstoreId")
	if !ok {
		log.AddContext(ctx).Infof("can not get vstoreId from response, set it to default %s", DefaultVStoreID)
		cli.VStoreID = DefaultVStoreID
	}

	log.AddContext(ctx).Infof("login %s success", cli.Url)
	return nil
}

// Logout logout
func (cli *RestClient) Logout(ctx context.Context) {
	resp, err := cli.BaseCall(ctx, "DELETE", "/sessions", nil)
	if err != nil {
		log.AddContext(ctx).Warningf("logout %s error: %v", cli.Url, err)
		return
	}

	code := int64(resp.Error["code"].(float64))
	if code != 0 {
		log.AddContext(ctx).Warningf("logout %s error: %d", cli.Url, code)
		return
	}

	log.AddContext(ctx).Infof("logout %s success", cli.Url)
}

// ReLogin logout and login again
func (cli *RestClient) ReLogin(ctx context.Context) error {
	oldToken := cli.Token

	cli.ReLoginMutex.Lock()
	defer cli.ReLoginMutex.Unlock()

	if cli.Token != "" && oldToken != cli.Token {
		// Coming here indicates other thread had already done relogin, so no need to relogin again
		return nil
	} else if cli.Token != "" {
		cli.Logout(ctx)
	}

	err := cli.Login(ctx)
	if err != nil {
		log.AddContext(ctx).Errorf("try to relogin error: %v", err)
		return err
	}

	return nil
}

// GetBackendID get backend id of client
func (cli *RestClient) GetBackendID() string {
	return cli.DeviceId
}

// GetDeviceSN used for get device sn
func (cli *RestClient) GetDeviceSN() string {
	return cli.DeviceId
}

// GetStorageVersion used for get storage version
func (cli *RestClient) GetStorageVersion() string {
	return cli.StorageVersion
}

// GetSystem used for get system info
func (cli *RestClient) GetSystem(ctx context.Context) (map[string]interface{}, error) {
	resp, err := cli.Get(ctx, "/system/", nil)
	if err != nil {
		return nil, err
	}

	code, msg, err := utils.FormatRespErr(resp.Error)
	if err != nil {
		return nil, err
	}

	if code != 0 {
		return nil, fmt.Errorf("get system info failed, error code: %d, error msg: %s", code, msg)
	}

	respData, ok := resp.Data.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("convert respData to map failed, data: %v", resp.Data)
	}

	return respData, nil
}

// ValidateLogin validates the login info
func (cli *RestClient) ValidateLogin(ctx context.Context) error {
	var resp Response
	var err error

	params, err := pkgUtils.GetAuthInfoFromSecret(ctx, cli.SecretName, cli.SecretNamespace)
	if err != nil {
		return err
	}

	data := map[string]interface{}{
		"username": cli.User,
		"password": params.Password,
		"scope":    params.Scope,
	}
	params.Password = ""

	cli.DeviceId = ""
	cli.Token = ""
	for i, url := range cli.Urls {
		cli.Url = url + "/deviceManager/rest"
		log.AddContext(ctx).Infof("try to login %s", cli.Url)
		resp, err = cli.BaseCall(ctx, "POST", "/xx/sessions", data)
		if err == nil {
			/* Sort the login Url to the last slot of san addresses, so that
			   if this connection error, next time will try other Url first. */
			cli.Urls[i], cli.Urls[len(cli.Urls)-1] = cli.Urls[len(cli.Urls)-1], cli.Urls[i]
			break
		} else if err.Error() != Unconnected {
			log.AddContext(ctx).Errorf("login %s error", cli.Url)
			break
		}

		log.AddContext(ctx).Warningf("login %s error due to connection failure, gonna try another Url", cli.Url)
	}

	if err != nil {
		return err
	}

	code, msg, err := utils.FormatRespErr(resp.Error)
	if err != nil {
		return fmt.Errorf("format login response data error: %w", err)
	}

	if code != 0 {
		return fmt.Errorf("validate login %s failed, error code: %d, error msg: %s", cli.Url, code, msg)
	}

	cli.setDeviceIdFromRespData(ctx, resp)

	log.AddContext(ctx).Infof("validate login %s success", cli.Url)
	return nil
}

func (cli *RestClient) setDeviceIdFromRespData(ctx context.Context, resp Response) {
	respData, ok := resp.Data.(map[string]interface{})
	if !ok {
		log.AddContext(ctx).Warningf("convert response data to map[string]interface{} failed, data type: [%T]",
			resp.Data)
	}

	cli.DeviceId, ok = utils.GetValue[string](respData, "deviceid")
	if !ok {
		log.AddContext(ctx).Warningf("can not convert deviceId type %T to string", respData["deviceid"])
	}

	cli.Token, ok = utils.GetValue[string](respData, "iBaseToken")
	if !ok {
		log.AddContext(ctx).Warningf("can not convert iBaseToken type %T to string", respData["iBaseToken"])
	}
}
