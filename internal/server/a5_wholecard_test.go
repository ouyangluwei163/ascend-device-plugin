/*
 * Copyright 2026 The HAMi Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"ascend-common/api"
	"ascend-common/devmanager"
	"ascend-common/devmanager/common"
	"ascend-common/devmanager/dcmi"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/util/client"
	"github.com/Project-HAMi/ascend-device-plugin/internal/manager"
)

// fakeA5DevManager 模拟一台 Atlas 850E 的 dcmi：8 张单芯 Ascend950DT。
// 只实现插件启动/注册路径会碰到的方法，其余走嵌入的 nil 接口（被调到即 panic，
// 等于断言"整卡路径不该碰到它们"）。
type fakeA5DevManager struct {
	devmanager.DeviceInterface
	cards int32
}

func (f *fakeA5DevManager) GetValidChipInfo() (common.ChipInfo, error) {
	// A5 的 dcmi 芯片名自带 Ascend 前缀（910B 系是裸的 "910B3"）。
	return common.ChipInfo{Type: "Ascend", Name: "Ascend950DT", Version: "V1"}, nil
}

func (f *fakeA5DevManager) GetDeviceList() (int32, []int32, error) {
	ids := make([]int32, f.cards)
	for i := range ids {
		ids[i] = int32(i)
	}
	return f.cards, ids, nil
}

func (f *fakeA5DevManager) GetPhysicIDFromLogicID(logicID int32) (int32, error) {
	return logicID, nil
}

func (f *fakeA5DevManager) GetDevType() string {
	return api.Ascend910A5
}

// 与真机 DeviceManagerV2 一致：dcmiv2 不支持按 logicID 取 card/device ID。
func (f *fakeA5DevManager) GetCardIDDeviceID(logicID int32) (int32, int32, error) {
	return -1, -1, fmt.Errorf("getCardIDDeviceID by logicID(%d) is not supported in dcmiv2", logicID)
}

// 与真机一致：A5 只认 DDIE，VDIE 返回 -8255 not supported。
func (f *fakeA5DevManager) GetDieID(logicID int32, dieType dcmi.DieType) (string, error) {
	if dieType != dcmi.DDIE {
		return "", fmt.Errorf("logicID(%d):get chip die ID info failed,error code: -8255", logicID)
	}
	return fmt.Sprintf("a5-die-%d", logicID), nil
}

func (f *fakeA5DevManager) GetDeviceHealth(int32) (uint32, error) {
	return 0, nil
}

// repoDeviceConfig 把仓库里发布的示例 configmap 的 device-config.yaml 落成文件，
// 让测试吃的就是用户实际 apply 的那份配置，而不是测试里手写的副本。
func repoDeviceConfig(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "ascend-device-configmap.yaml"))
	if err != nil {
		t.Fatalf("read repo configmap: %v", err)
	}
	var cm v1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("unmarshal repo configmap: %v", err)
	}
	path := filepath.Join(t.TempDir(), "device-config.yaml")
	if err := os.WriteFile(path, []byte(cm.Data["device-config.yaml"]), 0o600); err != nil {
		t.Fatalf("write device-config: %v", err)
	}
	return path
}

// S1：A5 节点以 Whole-card 方式被识别并注册给 HAMi——dcmi 报 Ascend950DT 时选中同名条目，
// 每张卡只暴露一个整卡设备，注册结果为 8 卡 × 98304 MB × 32 AI Core。
func TestA5WholeCard_HAMiRegistration(t *testing.T) {
	am := manager.NewAscendManagerWithDevice(&fakeA5DevManager{cards: 8})
	if err := am.LoadConfig(repoDeviceConfig(t)); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := am.UpdateDevice(); err != nil {
		t.Fatalf("UpdateDevice: %v", err)
	}
	if got := am.CommonWord(); got != "Ascend950DT" {
		t.Fatalf("CommonWord = %q, want Ascend950DT", got)
	}
	if got := am.ResourceName(); got != "huawei.com/Ascend950DT" {
		t.Fatalf("ResourceName = %q, want huawei.com/Ascend950DT", got)
	}

	ps, err := NewPluginServer(am, "a5-node", 0, false)
	if err != nil {
		t.Fatalf("NewPluginServer: %v", err)
	}
	// 每卡一个 kubelet 设备 == 整卡；有模板的芯片这里会是 卡数 × 切片数。
	if got := len(ps.apiDevices()); got != 8 {
		t.Fatalf("kubelet devices = %d, want 8 (one whole card per NPU)", got)
	}

	cleanup := setupFakeClient(nil, []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "a5-node", Annotations: map[string]string{}}},
	})
	defer cleanup()
	if err := ps.registerHAMi(); err != nil {
		t.Fatalf("registerHAMi: %v", err)
	}
	node, err := client.KubeClient.CoreV1().Nodes().Get(context.Background(), "a5-node", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	devs, err := device.UnMarshalNodeDevices(node.Annotations["hami.io/node-register-Ascend950DT"])
	if err != nil {
		t.Fatalf("unmarshal register annotation: %v", err)
	}
	if len(devs) != 8 {
		t.Fatalf("registered devices = %d, want 8", len(devs))
	}
	for i, d := range devs {
		if d.Type != "Ascend950DT" || d.Count != 1 || d.Devmem != 98304 || d.Devcore != 32 || !d.Health {
			t.Fatalf("device %d = {Type:%s Count:%d Devmem:%d Devcore:%d Health:%v}, want {Ascend950DT 1 98304 32 true}",
				i, d.Type, d.Count, d.Devmem, d.Devcore, d.Health)
		}
		// NetworkID 只给 910 系（HCCS 拓扑打分用）；A5 走 UB，带上它会把 HAMi 拐进 910 拓扑分支。
		if _, ok := d.CustomInfo["NetworkID"]; ok {
			t.Fatalf("device %d carries NetworkID, want none for A5", i)
		}
	}
}

// The selected DDIEs must reach allocation unchanged; a whole-card allocation
// must not inject a vNPU template, hooks or ENPU device mounts.
func TestA5WholeCardAllocation(t *testing.T) {
	am := manager.NewAscendManagerWithDevice(&fakeA5DevManager{cards: 8})
	if err := am.LoadConfig(repoDeviceConfig(t)); err != nil {
		t.Fatal(err)
	}
	if err := am.UpdateDevice(); err != nil {
		t.Fatal(err)
	}
	ps, err := NewPluginServer(am, "a5-node", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	selected := device.ContainerDevices{}
	for _, id := range []int{1, 3, 6} {
		selected = append(selected, device.ContainerDevice{UUID: fmt.Sprintf("a5-die-%d", id), Type: "Ascend950DT", Usedmem: 98304})
	}
	response, err := ps.buildContainerAllocateResponse(&v1.Pod{}, "worker", selected, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ASCEND_VISIBLE_DEVICES": "1,3,6"}
	if !reflect.DeepEqual(response.Envs, want) || len(response.Mounts) != 0 || len(response.Devices) != 0 {
		t.Fatalf("whole-card allocation = %+v, want env %v without slicing assets", response, want)
	}
}
