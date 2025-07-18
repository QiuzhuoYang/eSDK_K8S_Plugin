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

package plugin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Huawei/eSDK_K8S_Plugin/v4/csi/app"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/pkg/constants"
	pkgUtils "github.com/Huawei/eSDK_K8S_Plugin/v4/pkg/utils"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/storage/oceanstorage/oceanstor/client"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/storage/oceanstorage/oceanstor/clientv6"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/storage/oceanstorage/oceanstor/smartx"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/utils"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/utils/log"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/utils/version"
)

const (
	// DoradoV6PoolUsageType defines pool usage type of dorado v6
	DoradoV6PoolUsageType = "0"

	// ProtocolNfs defines protocol type nfs
	ProtocolNfs = "nfs"
	// ProtocolNfsPlus defines protocol type nfs+
	ProtocolNfsPlus = "nfs+"

	// SystemVStore default value is 0
	SystemVStore = "0"

	volumeNameSuffix = "-{{.PVCUid}}"
)

// OceanstorPlugin provides oceanstor plugin base operations
type OceanstorPlugin struct {
	basePlugin

	vStoreId string

	cli          client.OceanstorClientInterface
	product      constants.OceanstorVersion
	capabilities map[string]interface{}
}

func (p *OceanstorPlugin) init(ctx context.Context, config map[string]interface{}, keepLogin bool) error {
	backendClientConfig, err := formatOceanstorInitParam(config)
	if err != nil {
		return err
	}

	cli, err := client.NewClient(ctx, backendClientConfig)
	if err != nil {
		return err
	}

	if err = cli.Login(ctx); err != nil {
		log.AddContext(ctx).Errorf("plugin init login failed, err: %v", err)
		return err
	}

	if err = cli.SetSystemInfo(ctx); err != nil {
		cli.Logout(ctx)
		log.AddContext(ctx).Errorf("set client info failed, err: %v", err)
		return err
	}

	p.name = backendClientConfig.Name
	p.product = cli.Product

	if p.product.IsDoradoV6OrV7() {
		clientV6, err := clientv6.NewClientV6(ctx, backendClientConfig)
		if err != nil {
			cli.Logout(ctx)
			log.AddContext(ctx).Errorf("new OceanStor V6 client error: %v", err)
			return err
		}
		cli.Logout(ctx)
		clientV6.LastLif = cli.GetCurrentLif(ctx)
		clientV6.CurrentLifWwn = cli.CurrentLifWwn
		err = p.switchClient(ctx, clientV6)
		if err != nil {
			return err
		}
	} else {
		p.cli = cli
	}
	if !keepLogin {
		cli.Logout(ctx)
	}
	p.vStoreId = cli.VStoreID
	return nil
}

func (p *OceanstorPlugin) formatInitParam(config map[string]interface{}) (res *client.NewClientConfig, err error) {
	res = &client.NewClientConfig{}

	configUrls, exist := config["urls"].([]interface{})
	if !exist || len(configUrls) <= 0 {
		err = errors.New("urls must be provided")
		return
	}
	for _, i := range configUrls {
		res.Urls = append(res.Urls, i.(string))
	}
	res.User, exist = config["user"].(string)
	if !exist {
		err = errors.New("user must be provided")
		return
	}
	res.SecretName, exist = config["secretName"].(string)
	if !exist {
		err = errors.New("SecretName must be provided")
		return
	}
	res.SecretNamespace, exist = config["secretNamespace"].(string)
	if !exist {
		err = errors.New("SecretNamespace must be provided")
		return
	}
	res.BackendID, exist = config["backendID"].(string)
	if !exist {
		err = errors.New("backendID must be provided")
		return
	}
	res.VstoreName, _ = config["vstoreName"].(string)
	res.ParallelNum, _ = config["maxClientThreads"].(string)

	res.UseCert, _ = config["useCert"].(bool)
	res.CertSecretMeta, _ = config["certSecret"].(string)

	res.Storage, exist = config["storage"].(string)
	if !exist {
		return nil, errors.New("storage type must be configured for backend")
	}

	res.Name, exist = config["name"].(string)
	if !exist {
		return nil, errors.New("storage name must be configured for backend")
	}
	return
}

func (p *OceanstorPlugin) updateBackendCapabilities(ctx context.Context) (map[string]interface{}, error) {
	features, err := p.cli.GetLicenseFeature(ctx)
	if err != nil {
		log.Errorf("Get license feature error: %v", err)
		return nil, err
	}

	log.AddContext(ctx).Debugf("Get license feature: %v", features)

	supportThin := utils.IsSupportFeature(features, "SmartThin")
	supportThick := !p.product.IsDorado() && !p.product.IsDoradoV6OrV7()
	supportQoS := utils.IsSupportFeature(features, "SmartQoS")
	supportMetro := utils.IsSupportFeature(features, "HyperMetro")
	supportMetroNAS := utils.IsSupportFeature(features, "HyperMetroNAS")
	supportReplication := utils.IsSupportFeature(features, "HyperReplication")
	supportClone := utils.IsSupportFeature(features, "HyperClone") || utils.IsSupportFeature(features, "HyperCopy")
	supportApplicationType := p.product.IsDoradoV6OrV7()

	log.AddContext(ctx).Debugf("storageVersion: %v", p.cli.GetStorageVersion())

	capabilities := map[string]interface{}{
		"SupportThin":            supportThin,
		"SupportThick":           supportThick,
		"SupportQoS":             supportQoS,
		"SupportMetro":           supportMetro,
		"SupportReplication":     supportReplication,
		"SupportApplicationType": supportApplicationType,
		"SupportClone":           supportClone,
		"SupportMetroNAS":        supportMetroNAS,
	}

	return capabilities, nil
}

func (p *OceanstorPlugin) getRemoteDevices(ctx context.Context) (string, error) {
	devices, err := p.cli.GetAllRemoteDevices(ctx)
	if err != nil {
		log.AddContext(ctx).Errorf("Get remote devices error: %v", err)
		return "", err
	}

	var devicesSN []string
	for _, dev := range devices {
		deviceSN, ok := dev["SN"].(string)
		if !ok {
			continue
		}
		devicesSN = append(devicesSN, deviceSN)
	}
	return strings.Join(devicesSN, ";"), nil
}

func (p *OceanstorPlugin) updateBackendSpecifications(ctx context.Context) (map[string]interface{}, error) {
	devicesSN, err := p.getRemoteDevices(ctx)
	if err != nil {
		return nil, err
	}

	specifications := map[string]interface{}{
		"LocalDeviceSN":   p.cli.GetDeviceSN(),
		"RemoteDevicesSN": devicesSN,
		"VStoreID":        p.cli.GetvStoreID(),
		"VStoreName":      p.cli.GetvStoreName(),
	}
	return specifications, nil
}

// updateVStorePair update vStore pair info
func (p *OceanstorPlugin) updateVStorePair(ctx context.Context, specifications map[string]interface{}) {
	if specifications == nil {
		specifications = map[string]interface{}{}
	}

	// only Dorado V6 6.1.5 and later versions need to update vStorePair.
	if !p.product.IsDoradoV6OrV7() ||
		(p.product.IsDoradoV6() && version.CompareVersions(p.cli.GetStorageVersion(), constants.DoradoV615) == -1) ||
		p.cli.GetvStoreID() == "" {
		log.AddContext(ctx).Debugf("storage product is %s,version is %s, vStore id is %s, "+
			"do not update VStorePairId", p.product, p.cli.GetStorageVersion(), p.cli.GetvStoreID())
		return
	}

	vStorePairs, err := p.cli.GetVStorePairs(ctx)
	if err != nil {
		log.AddContext(ctx).Errorf("Get vStore pairs error: %v", err)
		return
	}

	if len(vStorePairs) == 0 {
		log.AddContext(ctx).Debugln("Get vStore pairs is empty")
		return
	}

	for _, pair := range vStorePairs {
		if data, ok := pair.(map[string]interface{}); ok {
			if localVStoreId, ok := data["LOCALVSTOREID"].(string); ok && localVStoreId == p.cli.GetvStoreID() {
				specifications["VStorePairId"] = data["ID"]
				specifications["HyperMetroDomainId"] = data["DOMAINID"]
				return
			}
		}
	}
	log.AddContext(ctx).Debugf("not found VStorePairId and HyperMetroDomainId, current vStoreId is %s",
		p.cli.GetvStoreID())
}

// for fileSystem on dorado storage, only Thin is supported
func (p *OceanstorNasPlugin) updateSmartThin(capabilities map[string]interface{}) error {
	if capabilities == nil {
		return nil
	}
	if p.product.IsDorado() || p.product.IsDoradoV6OrV7() {
		capabilities["SupportThin"] = true
	}
	return nil
}

// UpdateBackendCapabilities used to update backend capabilities
func (p *OceanstorPlugin) UpdateBackendCapabilities(ctx context.Context) (map[string]interface{},
	map[string]interface{}, error) {
	capabilities, err := p.updateBackendCapabilities(ctx)
	if err != nil {
		log.AddContext(ctx).Errorf("updateBackendCapabilities failed, err: %v", err)
		return nil, nil, err
	}

	specifications, err := p.updateBackendSpecifications(ctx)
	if err != nil {
		log.AddContext(ctx).Errorf("updateBackendSpecifications failed, err: %v", err)
		return nil, nil, err
	}
	p.capabilities = capabilities
	return capabilities, specifications, nil
}

func getParams(ctx context.Context, name string,
	parameters map[string]interface{}) map[string]interface{} {

	params := map[string]interface{}{
		"name":        name,
		"description": parameters["description"].(string),
		"capacity":    utils.RoundUpSize(parameters["size"].(int64), constants.AllocationUnitBytes),
		"vstoreId":    "0",
	}

	resetParams(parameters, params)
	toLowerParams(parameters, params)
	processBoolParams(ctx, parameters, params)
	return params
}

// resetParams process need reset param
func resetParams(source, target map[string]interface{}) {
	if source == nil || target == nil {
		return
	}
	if fileSystemName, ok := source["annVolumeName"]; ok {
		target["name"] = fileSystemName
	}

	if advancedOptions, ok := utils.GetValue[string](source, constants.AdvancedOptionsKey); ok {
		target[constants.AdvancedOptionsKey] = advancedOptions
	}
}

// processBoolParams process bool param
func processBoolParams(ctx context.Context, source, target map[string]interface{}) {
	if source == nil || target == nil {
		return
	}
	// Add new bool parameter here
	for _, i := range []string{
		"replication",
		"hyperMetro",
	} {
		if v, exist := source[i].(string); exist && v != "" {
			target[strings.ToLower(i)] = utils.StrToBool(ctx, v)
		}
	}
}

// toLowerParams convert params to lower
func toLowerParams(source, target map[string]interface{}) {
	if source == nil || target == nil {
		return
	}
	for _, key := range []string{
		"storagepool",
		"allocType",
		"qos",
		"authClient",
		"backend",
		"cloneFrom",
		"cloneSpeed",
		"metroDomain",
		"remoteStoragePool",
		"sourceSnapshotName",
		"sourceVolumeName",
		"snapshotParentId",
		"applicationType",
		"allSquash",
		"rootSquash",
		"fsPermission",
		"snapshotDirectoryVisibility",
		"reservedSnapshotSpaceRatio",
		"parentname",
		"vstoreId",
		"replicationSyncPeriod",
		"vStorePairID",
		"accesskrb5",
		"accesskrb5i",
		"accesskrb5p",
		"fileSystemMode",
		"metroPairSyncSpeed",
	} {
		if v, exist := source[key]; exist && v != "" {
			target[strings.ToLower(key)] = v
		}
	}
}

func (p *OceanstorPlugin) updatePoolCapabilities(ctx context.Context, poolNames []string,
	vStoreQuotaMap map[string]interface{}, usageType string) (map[string]interface{}, error) {
	pools, err := p.cli.GetAllPools(ctx)
	if err != nil {
		log.AddContext(ctx).Errorf("Get all pools error: %v", err)
		return nil, err
	}

	log.AddContext(ctx).Debugf("Get pools: %v", pools)

	var validPools []map[string]interface{}
	for _, name := range poolNames {
		if pool, exist := pools[name].(map[string]interface{}); exist {
			poolType, exist := pool["NEWUSAGETYPE"].(string)
			if (pool["USAGETYPE"] == usageType || pool["USAGETYPE"] == DoradoV6PoolUsageType) ||
				(exist && poolType == DoradoV6PoolUsageType) {
				validPools = append(validPools, pool)
			} else {
				log.AddContext(ctx).Warningf("Pool %s is not for %s", name, usageType)
			}
		} else {
			log.AddContext(ctx).Warningf("Pool %s does not exist", name)
		}
	}

	capabilities := analyzePoolsCapacity(ctx, validPools, vStoreQuotaMap)
	return capabilities, nil
}

// SupportQoSParameters checks requested QoS parameters support by Oceanstor plugin
func (p *OceanstorPlugin) SupportQoSParameters(ctx context.Context, qosConfig string) error {
	return smartx.CheckQoSParameterSupport(ctx, p.product, qosConfig)
}

// Logout is to logout the storage session
func (p *OceanstorPlugin) Logout(ctx context.Context) {
	if p.cli != nil {
		p.cli.Logout(ctx)
	}
}

// ReLogin will refresh the user session of storage
func (p *OceanstorPlugin) ReLogin(ctx context.Context) error {
	if p.cli == nil {
		return nil
	}

	return p.cli.ReLogin(ctx)
}

// GetSectorSize get sector size of plugin
func (p *OceanstorPlugin) GetSectorSize() int64 {
	return SectorSize
}

func (p *OceanstorPlugin) switchClient(ctx context.Context, newClient client.OceanstorClientInterface) error {
	log.AddContext(ctx).Infoln("Using OceanStor V6 or Dorado V6 client.")
	p.cli = newClient
	err := p.cli.Login(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			p.Logout(ctx)
		}
	}()
	// If a new URL is selected when the client is switched, the system information needs to be updated.
	if err = p.cli.SetSystemInfo(ctx); err != nil {
		log.AddContext(ctx).Errorf("set system info failed, error: %v", err)
		return err
	}
	return nil
}

var (
	pvcNamespaceRe = regexp.MustCompile(`\{\{\s*\.PVCNamespace\s*\}\}`)
	pvcNameRe      = regexp.MustCompile(`\{\{\s*\.PVCName\s*\}\}`)
)

func validateVolumeName(volumeNameTpl string) error {
	if !pvcNamespaceRe.MatchString(volumeNameTpl) || !pvcNameRe.MatchString(volumeNameTpl) {
		return errors.New("{{.PVCNamespace}} or {{." +
			"PVCName}} must be configured in the volumeName parameter at the same time")
	}

	return nil
}

func newExtraCreateMetadataFromParameters(parameters map[string]any) (map[string]string, error) {
	for _, key := range []string{constants.PVCNamespaceKey, constants.PVCNameKey, constants.PVNameKey} {
		if _, exist := parameters[key]; !exist {
			return nil, fmt.Errorf("metadata key %s not found", key)
		}
	}

	pvcNamespace, _ := utils.GetValue[string](parameters, constants.PVCNamespaceKey)
	pvcName, _ := utils.GetValue[string](parameters, constants.PVCNameKey)
	pvName, _ := utils.GetValue[string](parameters, constants.PVNameKey)
	pvcUid := strings.TrimPrefix(pvName, app.GetGlobalConfig().VolumeNamePrefix)
	pvcUid = strings.ReplaceAll(pvcUid, "-", "")

	return map[string]string{
		"PVCNamespace": pvcNamespace,
		"PVCName":      pvcName,
		"PVName":       pvName,
		"PVCUid":       pvcUid,
	}, nil
}

func getNewClientConfig(ctx context.Context, param map[string]interface{}) (*client.NewClientConfig, error) {
	data := &client.NewClientConfig{}
	configUrls, exist := param["urls"].([]interface{})
	if !exist || len(configUrls) <= 0 {
		return data, fmt.Errorf("verify urls: [%v] failed. urls must be provided", param["urls"])
	}
	for _, configUrl := range configUrls {
		url, ok := configUrl.(string)
		if !ok {
			return data, fmt.Errorf("verify url: [%v] failed. url convert to string failed", configUrl)
		}
		data.Urls = append(data.Urls, url)
	}

	var urls []string
	for _, i := range configUrls {
		urls = append(urls, i.(string))
	}

	err := checkClientConfig(param, data)
	if err != nil {
		return data, err
	}

	return data, nil
}

// SetCli sets the cli for Oceanstor Plugin
func (p *OceanstorPlugin) SetCli(cli client.OceanstorClientInterface) {
	p.cli = cli
}

// SetProduct sets the product for Oceanstor Plugin
func (p *OceanstorPlugin) SetProduct(product constants.OceanstorVersion) {
	p.product = product
}

// checkClientConfig used to check the param
func checkClientConfig(param map[string]interface{}, data *client.NewClientConfig) error {
	var ok bool
	data.User, ok = utils.GetValue[string](param, "user")
	if !ok {
		return fmt.Errorf("verify user: [%v] failed. user must be provided", data.User)
	}

	data.SecretName, ok = utils.GetValue[string](param, "secretName")
	if !ok {
		return fmt.Errorf("verify SecretName: [%v] failed. SecretName must be provided", data.SecretName)
	}

	data.SecretNamespace, ok = utils.GetValue[string](param, "secretNamespace")
	if !ok {
		return fmt.Errorf("verify SecretNamespace: [%v] failed. SecretNamespace must be provided", data.SecretNamespace)
	}

	data.BackendID, ok = utils.GetValue[string](param, "backendID")
	if !ok {
		return fmt.Errorf("verify backendID: [%v] failed. backendID must be provided", param["backendID"])
	}

	data.AuthenticationMode, ok = utils.GetValue[string](param, constants.AuthenticationModeKey)
	if ok {
		err := pkgUtils.CheckAuthenticationMode(data.AuthenticationMode)
		if err != nil {
			return fmt.Errorf("verify AuthenticationMode: [%s] failed, err: %w", data.AuthenticationMode, err)
		}
	}

	data.VstoreName, _ = utils.GetValue[string](param, "vstoreName")
	data.ParallelNum, _ = utils.GetValue[string](param, "maxClientThreads")
	data.UseCert, _ = utils.GetValue[bool](param, "useCert")
	data.CertSecretMeta, _ = utils.GetValue[string](param, "certSecret")
	return nil
}
