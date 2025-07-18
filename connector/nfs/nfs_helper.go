/*
 *  Copyright (c) Huawei Technologies Co., Ltd. 2020-2024. All rights reserved.
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

// Package nfs to mount or unmount filesystem
package nfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/Huawei/eSDK_K8S_Plugin/v4/connector"
	connUtils "github.com/Huawei/eSDK_K8S_Plugin/v4/connector/utils"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/pkg/constants"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/utils"
	"github.com/Huawei/eSDK_K8S_Plugin/v4/utils/log"
)

const (
	fsInfoSegment             = 2
	targetMountPathPermission = 0750
	unformattedFsCode         = 2
)

type connectorInfo struct {
	srcType    string
	sourcePath string
	targetPath string
	fsType     string
	mntFlags   connUtils.MountParam
	accessMode csi.VolumeCapability_AccessMode_Mode
}

func parseNFSInfo(ctx context.Context,
	connectionProperties map[string]interface{}) (*connectorInfo, error) {
	var con connectorInfo
	srcType, typeExist := connectionProperties["srcType"].(string)
	if !typeExist || srcType == "" {
		msg := "there are no srcType in the connection info"
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	sourcePath, srcPathExist := connectionProperties["sourcePath"].(string)
	if !srcPathExist || sourcePath == "" {
		msg := "there are no source path in the connection info"
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	targetPath, tgtPathExist := connectionProperties["targetPath"].(string)
	if !tgtPathExist || targetPath == "" {
		msg := "there are no target path in the connection info"
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	fsType, _ := connectionProperties["fsType"].(string)
	if fsType == "" {
		fsType = "ext4"
	}

	accessMode, _ := connectionProperties["accessMode"].(csi.VolumeCapability_AccessMode_Mode)
	mntDashO, _ := connectionProperties["mountFlags"].(string)
	protocol, _ := connectionProperties["protocol"].(string)
	var mntDashT string
	if protocol == constants.ProtocolDpc {
		mntDashT = constants.ProtocolDpc
	}

	if protocol == constants.ProtocolDtfs {
		mntDashT = constants.ProtocolDtfs
	}

	con.srcType = srcType
	con.sourcePath = sourcePath
	con.targetPath = targetPath
	con.fsType = fsType
	con.accessMode = accessMode
	con.mntFlags = connUtils.MountParam{DashO: strings.TrimSpace(mntDashO), DashT: mntDashT}

	return &con, nil
}

func tryConnectVolume(ctx context.Context, connMap map[string]interface{}) (string, error) {
	conn, err := parseNFSInfo(ctx, connMap)
	if err != nil {
		return "", err
	}

	switch conn.srcType {
	case "block":
		_, err = connector.ReadDevice(ctx, conn.sourcePath)
		if err != nil {
			return "", err
		}

		err = mountDisk(ctx, conn)
		if err != nil {
			return "", err
		}
	case "fs":
		err = mountFS(ctx, conn.sourcePath, conn.targetPath, conn.mntFlags)
		if err != nil {
			return "", err
		}
	default:
		return "", errors.New("not support source type")
	}
	return "", nil
}

func preMount(sourcePath, targetPath string, checkSourcePath bool) error {
	if checkSourcePath {
		if _, err := os.Stat(sourcePath); err != nil && os.IsNotExist(err) {
			return errors.New("source path does not exist")
		}
	}

	if _, err := os.Stat(targetPath); err != nil && os.IsNotExist(err) {
		if err := os.MkdirAll(targetPath, targetMountPathPermission); err != nil {
			return errors.New("can not create a target path")
		}
	}

	return nil
}

func mountFS(ctx context.Context, sourcePath, targetPath string, flags connUtils.MountParam) error {
	return connUtils.MountToDir(ctx, sourcePath, targetPath, flags, false)
}

func getFSType(ctx context.Context, sourcePath string) (string, error) {
	// the errorCode 2 means an unFormatted filesystem and the unavailable filesystem. So ensure the device is
	// available before calling command blkid
	if exist, err := utils.PathExist(sourcePath); !exist {
		return "", fmt.Errorf("find the device %s failed before get filesystem info, error: %v", sourcePath, err)
	}

	output, err := utils.ExecShellCmd(ctx, "blkid -o udev %s", sourcePath)
	if err != nil {
		if errCode, ok := err.(*exec.ExitError); ok && errCode.ExitCode() == unformattedFsCode {
			log.AddContext(ctx).Infof("Query fs of %s, output: %s, error: %s", sourcePath, output, err)
			if formatted, err := connector.IsDeviceFormatted(ctx, sourcePath); err != nil {
				return "", fmt.Errorf("check device %s formatted failed, error: %v", sourcePath, err)
			} else if formatted {
				return "", fmt.Errorf("1. Maybe the device %s is formatted; 2. Maybe the device is a "+
					"raw block volume, please check. error: %v", sourcePath, err)
			}

			return "", nil
		}
		return "", err
	}

	for _, out := range strings.Split(output, "\n") {
		fsInfo := strings.Split(out, "=")
		if len(fsInfo) == fsInfoSegment && fsInfo[0] == "ID_FS_TYPE" {
			return fsInfo[1], nil
		}
	}

	return "", errors.New("get fsType failed")
}

func formatDisk(ctx context.Context, sourcePath, fsType, diskSizeType string) error {
	var cmd string
	if fsType == "xfs" {
		cmd = fmt.Sprintf("mkfs -t %s -f %s", fsType, sourcePath)
	} else {
		// Handle ext types
		switch diskSizeType {
		case "default":
			cmd = fmt.Sprintf("mkfs -t %s -F %s", fsType, sourcePath)
		case "big":
			cmd = fmt.Sprintf("mkfs -t %s -T big -F %s", fsType, sourcePath)
		case "huge":
			cmd = fmt.Sprintf("mkfs -t %s -T huge -F %s", fsType, sourcePath)
		case "large":
			cmd = fmt.Sprintf("mkfs -t %s -T largefile -F %s", fsType, sourcePath)
		case "veryLarge":
			cmd = fmt.Sprintf("mkfs -t %s -T largefile4 -F %s", fsType, sourcePath)
		default:
			return fmt.Errorf("%v:%v not found", "diskSizeType", diskSizeType)
		}
	}

	output, err := utils.ExecShellCmd(ctx, cmd)
	if err != nil {
		if strings.Contains(output, "in use by the system") {
			log.AddContext(ctx).Infof("The disk %s is in formatting, wait for 10 second", sourcePath)
			time.Sleep(time.Second * formatWaitInternal)
			return errors.New("the disk is in formatting, please wait")
		}
		log.AddContext(ctx).Errorf("Couldn't mkfs %s to %s: %s", sourcePath, fsType, output)
		return err
	}
	return nil
}

func getDiskSizeType(ctx context.Context, sourcePath string) (string, error) {
	size, err := connector.GetDeviceSize(ctx, sourcePath)
	if err != nil {
		log.AddContext(ctx).Errorf("Failed to get size from %s, error is %s", sourcePath, err)
		return "", err
	}

	log.AddContext(ctx).Infof("Get disk %s's size: %d", sourcePath, size)
	if size <= halfTiSizeBytes {
		return "default", nil
	} else if size > halfTiSizeBytes && size <= oneTiSizeBytes {
		return "big", nil
	} else if size > oneTiSizeBytes && size <= tenTiSizeBytes {
		return "huge", nil
	} else if size > tenTiSizeBytes && size <= hundredTiSizeBytes {
		return "large", nil
	} else if size > hundredTiSizeBytes && size <= halfPiSizeBytes {
		return "veryLarge", nil
	}

	// if the size bigger than 512TiB, mark it is a large disk, more info: /etc/mke2fs.conf
	return "", errors.New("the disk size does not support")
}

func mountDisk(ctx context.Context, conn *connectorInfo) error {
	var err error
	existFsType, err := getFSType(ctx, conn.sourcePath)
	if err != nil {
		return err
	}

	if existFsType == "" {
		// check this disk is in formatting
		inFormatting, err := connector.IsInFormatting(ctx, conn.sourcePath, conn.fsType)
		if err != nil {
			return err
		}

		if inFormatting {
			log.AddContext(ctx).Infof("Device %s is in formatting, no need format again. Wait 10 seconds", conn.sourcePath)
			time.Sleep(time.Second * formatWaitInternal)
			return errors.New("the disk is in formatting, please wait")
		}

		diskSizeType, err := getDiskSizeType(ctx, conn.sourcePath)
		if err != nil {
			return err
		}

		err = formatDisk(ctx, conn.sourcePath, conn.fsType, diskSizeType)
		if err != nil {
			return err
		}

		err = connUtils.MountToDir(ctx, conn.sourcePath, conn.targetPath, conn.mntFlags, true)
		if err != nil {
			return err
		}
	} else {
		err = connUtils.MountToDir(ctx, conn.sourcePath, conn.targetPath, conn.mntFlags, true)
		if err != nil {
			return err
		}

		if conn.accessMode == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER {
			log.AddContext(ctx).Infoln("PVC accessMode is ReadWriteMany, not support to expend filesystem")
			return nil
		}

		if conn.accessMode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
			log.AddContext(ctx).Infoln("PVC accessMode is ReadOnlyMany, no need to expend filesystem")
			return nil
		}

		err = connector.ResizeMountPath(ctx, conn.targetPath)
		if err != nil {
			log.AddContext(ctx).Errorf("Resize mount path %s err %s", conn.targetPath, err)
			return err
		}
	}
	return nil
}

func removeTargetPath(targetPath string) error {
	_, err := os.Stat(targetPath)
	if err != nil && os.IsNotExist(err) {
		return nil
	}

	if err != nil && !os.IsNotExist(err) {
		msg := fmt.Sprintf("get target path %s state error %v", targetPath, err)
		log.Errorln(msg)
		return errors.New(msg)
	}

	if err := os.RemoveAll(targetPath); err != nil {
		msg := fmt.Sprintf("remove target path %s error %v", targetPath, err)
		log.Errorln(msg)
		return errors.New(msg)
	}
	return nil
}

func tryDisConnectVolume(ctx context.Context, targetPath string) error {
	err := connUtils.Unmount(ctx, targetPath)
	if err != nil {
		return err
	}

	return removeTargetPath(targetPath)
}
