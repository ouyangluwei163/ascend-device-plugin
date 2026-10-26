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

package internal

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// 本仓库发布两份 device-config：kubectl 用的示例 configmap 与 helm chart 的 values。
// 两份手工同步，新增芯片时最容易只改一边——kubectl 部署能起、helm 部署却报
// "can not find vnpu config"。这里钉住 A5 条目在两份里逐字段一致。
//
// 只比 A5 条目而非整表：既有芯片在两份里本就不同（chart 不开 vcannRT、910C 无模板），
// 那是部署形态差异，不在本用例的约束范围内。
func TestDeviceConfigCopiesInSync_A5(t *testing.T) {
	var cm v1.ConfigMap
	readYAML(t, filepath.Join("..", "ascend-device-configmap.yaml"), &cm)
	var fromConfigMap Config
	if err := yaml.Unmarshal([]byte(cm.Data["device-config.yaml"]), &fromConfigMap); err != nil {
		t.Fatalf("unmarshal configmap device-config: %v", err)
	}

	var values struct {
		DeviceConfig string `json:"deviceConfig"`
	}
	readYAML(t, filepath.Join("..", "charts", "ascend-device-plugin", "values.yaml"), &values)
	// The chart evaluates mode and quota expressions with tpl. Only decode the
	// literal per-chip list here; rendered chart checks cover the global fields.
	_, configs, ok := strings.Cut(values.DeviceConfig, "  configs:\n")
	if !ok {
		t.Fatal("chart deviceConfig has no configs list")
	}
	var fromChart Config
	if err := yaml.Unmarshal([]byte("vnpus:\n  configs:\n"+configs), &fromChart); err != nil {
		t.Fatalf("unmarshal chart deviceConfig: %v", err)
	}

	want := VNPUConfig{
		ChipName:           "Ascend950DT",
		CommonWord:         "Ascend950DT",
		ResourceName:       "huawei.com/Ascend950DT",
		ResourceMemoryName: "huawei.com/Ascend950DT-memory",
		MemoryAllocatable:  98304,
		MemoryCapacity:     98304,
		AICore:             32,
	}
	for name, cfg := range map[string]Config{"configmap": fromConfigMap, "chart": fromChart} {
		got, ok := findChip(cfg, want.ChipName)
		if !ok {
			t.Fatalf("%s: no %s entry", name, want.ChipName)
		}
		// 整卡芯片：不配模板、aiCPU，不开 vcannRT。
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %s entry = %+v, want %+v", name, want.ChipName, got, want)
		}
	}
}

func findChip(cfg Config, chipName string) (VNPUConfig, bool) {
	for _, c := range cfg.VNPUs.Configs {
		if c.ChipName == chipName {
			return c, true
		}
	}
	return VNPUConfig{}, false
}

func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}
