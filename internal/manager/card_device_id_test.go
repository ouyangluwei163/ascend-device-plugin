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

package manager

import (
	"errors"
	"testing"

	"ascend-common/api"
	"ascend-common/devmanager"
	"ascend-common/devmanager/dcmi"

	"github.com/Project-HAMi/ascend-device-plugin/internal"
)

// dcmiV2Mock 模拟 A5 的 DeviceManagerV2：GetCardIDDeviceID 不受支持。
type dcmiV2Mock struct {
	devmanager.DeviceManagerMock
}

func (d *dcmiV2Mock) GetCardIDDeviceID(logicID int32) (int32, int32, error) {
	return -1, -1, errors.New("getCardIDDeviceID is not supported in dcmiv2")
}

func (d *dcmiV2Mock) GetDieID(logicID int32, dieType dcmi.DieType) (string, error) {
	if dieType != dcmi.DDIE {
		return "", errors.New("get chip die ID info failed, error code: -8255")
	}
	return "DDIE-UUID", nil
}

func (d *dcmiV2Mock) GetPhysicIDFromLogicID(logicID int32) (int32, error) {
	return logicID + 4, nil
}

func TestCardAndDeviceID(t *testing.T) {
	t.Run("A5 falls back to (phyID, 0)", func(t *testing.T) {
		mock := &dcmiV2Mock{}
		mock.DevType = api.Ascend910A5
		am := NewAscendManagerWithDevice(mock)
		card, chip, err := am.cardAndDeviceID(2)
		if err != nil || card != 6 || chip != 0 {
			t.Fatalf("got (%d, %d, %v), want (6, 0, nil)", card, chip, err)
		}
	})
	t.Run("non-A5 uses dcmi card/device id", func(t *testing.T) {
		mock := &devmanager.DeviceManagerMock{}
		mock.DevType = api.Ascend910B
		am := NewAscendManagerWithDevice(mock)
		card, chip, err := am.cardAndDeviceID(0)
		if err != nil || card != 0 || chip != 0 {
			t.Fatalf("got (%d, %d, %v), want (0, 0, nil)", card, chip, err)
		}
	})
}

func TestUUIDOf_A5UsesDDIE(t *testing.T) {
	mock := &dcmiV2Mock{}
	mock.DevType = api.Ascend910A5
	am := NewAscendManagerWithDevice(mock)
	uuid, err := am.uuidOf(0)
	if err != nil || uuid != "DDIE-UUID" {
		t.Fatalf("got (%q, %v), want (DDIE-UUID, nil)", uuid, err)
	}
}

func TestA5AlwaysUsesWholeCards(t *testing.T) {
	for _, mode := range []string{"", internal.VNPUModeTemplate, internal.VNPUModeHamiCore, internal.VNPUModeENPU} {
		t.Run(mode, func(t *testing.T) {
			am := &AscendManager{
				config:       internal.VNPUConfig{ChipName: internal.Ascend950DT},
				globalConfig: internal.Config{VNPUs: internal.VNPUsConfig{HamiVnpuMode: mode, DeviceSplitCount: 10}},
				nodeConfig:   &internal.NodeConfig{HamiVnpuMode: mode, VDeviceCount: 20},
			}
			if am.IsHamiVnpuCore() || am.IsEnpu() || am.VDeviceCount() != 1 {
				t.Fatalf("mode %q enables slicing on A5: core=%v ENPU=%v slots=%d", mode, am.IsHamiVnpuCore(), am.IsEnpu(), am.VDeviceCount())
			}
		})
	}
}
