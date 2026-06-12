# Auto device-share Management (OSS) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically run `npu-smi set -t device-share -d 1` on each chip assigned to a `hami-core` Pod at Allocate time, so operators no longer enable device-share by hand.

**Architecture:** A new `internal/server/device_share.go` ports the commercial npu-smi helpers, slimmed to drop the commercial dual-label gate. `Allocate()` collects the distinct chips for the containers in the current call, and — only when `huawei.com/vnpu-mode == hami-core` — flips device-share once before erasing the to-allocate annotation. Fail-fast: any npu-smi error fails the Allocate.

**Tech Stack:** Go, `k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1`, `os/exec` (npu-smi), Go `testing` with the existing `FakeManager` + fake clientset harness.

**Design spec:** `docs/superpowers/specs/2026-06-12-oss-auto-device-share-design.md`

**Prerequisite:** submodules checked out (`git submodule update --init --recursive`) so the `manager` package and its `ascend-common` dependency build.

---

## File Structure

- **Create** `internal/server/device_share.go` — `chipKey`, `npuSmiCandidates` (var), `runNpuSmi` (var), `resolveNpuSmi`, `applyDeviceShare`. One responsibility: resolve and drive npu-smi device-share.
- **Create** `internal/server/device_share_test.go` — unit tests for the above (ported from commercial, minus the dropped label gate).
- **Modify** `internal/server/server.go` — `Allocate()` gains chip collection + the gated flip.
- **Modify** `internal/server/server_test.go` — add `TestAllocate_DeviceShare` and a `reflect` import.
- **Modify** `README.md` / `README_cn.md` — device-share is now auto-managed for `hami-core` Pods.

`VNPUModeHamiCore` and `VNPUModeAnnotation` already exist in `server.go` (consts, lines ~46-47); no new const needed.

---

## Task 1: npu-smi device-share helpers (`device_share.go`)

**Files:**
- Create: `internal/server/device_share.go`
- Test: `internal/server/device_share_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/server/device_share_test.go`:

```go
/*
 * Copyright 2024 The HAMi Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withFakeNpuSmi swaps the package-level runNpuSmi for a fake and restores it
// after the test. Shared by the Allocate device-share tests in server_test.go.
func withFakeNpuSmi(t *testing.T, fn func(args ...string) ([]byte, error)) {
	t.Helper()
	orig := runNpuSmi
	runNpuSmi = fn
	t.Cleanup(func() { runNpuSmi = orig })
}

func sampleChips() []chipKey {
	return []chipKey{
		{Card: 0, Chip: 0},
		{Card: 0, Chip: 1},
		{Card: 1, Chip: 0},
	}
}

func TestApplyDeviceShare_Enable(t *testing.T) {
	var calls [][]string
	withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	})

	if err := applyDeviceShare(sampleChips(), true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := [][]string{
		{"set", "-t", "device-share", "-i", "0", "-c", "0", "-d", "1"},
		{"set", "-t", "device-share", "-i", "0", "-c", "1", "-d", "1"},
		{"set", "-t", "device-share", "-i", "1", "-c", "0", "-d", "1"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls mismatch:\ngot  %v\nwant %v", calls, want)
	}
}

func TestApplyDeviceShare_Disable(t *testing.T) {
	var calls [][]string
	withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	})

	if err := applyDeviceShare(sampleChips(), false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != len(sampleChips()) {
		t.Fatalf("expected %d calls, got %d: %v", len(sampleChips()), len(calls), calls)
	}
	for _, c := range calls {
		if c[len(c)-1] != "0" {
			t.Fatalf("expected -d 0 on every call, got %v", c)
		}
	}
}

// TestApplyDeviceShare_FailFast guards the contract that the first per-chip
// failure stops the loop — Allocate cannot return a partial response, so any
// chip past the failure point must be left untouched and re-driven by the
// next Allocate.
func TestApplyDeviceShare_FailFast(t *testing.T) {
	var calls [][]string
	withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		// Fail on card=0 chip=1 (the second call).
		if len(args) >= 7 && args[4] == "0" && args[6] == "1" {
			return []byte("E80001 not allowed"), fmt.Errorf("exit status 1")
		}
		return nil, nil
	})

	err := applyDeviceShare(sampleChips(), true)
	if err == nil {
		t.Fatal("expected error from per-chip failure, got nil")
	}
	if !strings.Contains(err.Error(), "-i 0") || !strings.Contains(err.Error(), "-c 1") {
		t.Fatalf("error should identify failing chip via npu-smi flags, got: %v", err)
	}
	if got := len(calls); got != 2 {
		t.Fatalf("expected loop to stop after first failure (2 calls), got %d: %v", got, calls)
	}
}

func TestApplyDeviceShare_NoChips(t *testing.T) {
	called := false
	withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := applyDeviceShare(nil, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("npu-smi should not be invoked when chip list is empty")
	}
}

// TestRunNpuSmi_AnswersDeviceShareConfirmation guards the production runNpuSmi
// path: enabling device-share triggers npu-smi's interactive Y/N prompt and
// the binary exits 200 if stdin is closed. The fake here mimics that prompt
// and only succeeds when "Y" is delivered on stdin, so the test exercises the
// real exec.Command wiring (not the runNpuSmi mock used elsewhere).
func TestRunNpuSmi_AnswersDeviceShareConfirmation(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "npu-smi")
	script := "#!/bin/sh\n" +
		"echo 'There are security risks when opening device sharing,'\n" +
		"echo 'Are you sure you want to continue setting?(Y/N)'\n" +
		"read ans\n" +
		"[ \"$ans\" = \"Y\" ] || exit 200\n" +
		"echo ok\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake npu-smi: %v", err)
	}

	saved := npuSmiCandidates
	npuSmiCandidates = []string{fake}
	t.Cleanup(func() { npuSmiCandidates = saved })

	out, err := runNpuSmi("set", "-t", "device-share", "-i", "0", "-c", "0", "-d", "1")
	if err != nil {
		t.Fatalf("expected success after Y answer, got err=%v out=%q", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("expected confirmation to be consumed and command to print ok, got %q", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (do not compile)**

Run: `go test ./internal/server/ -run 'TestApplyDeviceShare|TestRunNpuSmi' -v`
Expected: build failure — `undefined: runNpuSmi`, `undefined: chipKey`, `undefined: applyDeviceShare`, `undefined: npuSmiCandidates`.

- [ ] **Step 3: Write the implementation**

Create `internal/server/device_share.go`:

```go
/*
 * Copyright 2024 The HAMi Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package server

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

// chipKey identifies a single NPU chip by the two coordinates npu-smi takes
// for its -i (card) and -c (chip) flags.
type chipKey struct {
	Card int32
	Chip int32
}

// npuSmiCandidates lists the host paths where npu-smi may live, in priority
// order. The first is what the daemonset's /usr/local/Ascend/driver hostPath
// mount exposes; the others cover hosts that ship npu-smi elsewhere. It is a
// package var so tests can point it at a temp dir.
var npuSmiCandidates = []string{
	"/usr/local/Ascend/driver/tools/npu-smi",
	"/usr/local/sbin/npu-smi",
	"/usr/local/bin/npu-smi",
}

// runNpuSmi executes npu-smi with the given args and returns combined output.
// It is a package var so tests can substitute a fake.
//
// Enabling device-share (-d 1) prints a "There are security risks ... continue
// setting?(Y/N)" prompt and aborts with exit 200 if stdin is closed. npu-smi
// has no -y/--force flag in the driver versions this plugin targets, so we
// feed "Y\n" unconditionally: commands that don't prompt simply ignore the
// unread stdin.
var runNpuSmi = func(args ...string) ([]byte, error) {
	bin, err := resolveNpuSmi()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader("Y\n")
	return cmd.CombinedOutput()
}

func resolveNpuSmi() (string, error) {
	for _, p := range npuSmiCandidates {
		st, err := os.Stat(p)
		if err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			return p, nil
		}
	}
	if p, err := exec.LookPath("npu-smi"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("npu-smi not found in %v or PATH", npuSmiCandidates)
}

// applyDeviceShare flips device-share on every chip in chips. It does not query
// current state first — npu-smi accepts redundant set commands, so the
// unconditional write is cheaper than a query+set round trip and keeps Allocate
// latency predictable.
//
// Fails fast on the first per-chip error. The caller (Allocate) propagates that
// error so kubelet surfaces it on the Pod; partial state on the remaining chips
// will be re-driven by the next Allocate that lands on them.
//
// This plugin only ever calls it with enabled=true; the enabled=false path
// exists for symmetry and test coverage.
func applyDeviceShare(chips []chipKey, enabled bool) error {
	if len(chips) == 0 {
		return nil
	}
	flag := "0"
	if enabled {
		flag = "1"
	}
	for _, c := range chips {
		card := strconv.Itoa(int(c.Card))
		chip := strconv.Itoa(int(c.Chip))
		out, err := runNpuSmi("set", "-t", "device-share", "-i", card, "-c", chip, "-d", flag)
		if err != nil {
			return fmt.Errorf("npu-smi set device-share -i %s -c %s -d %s: %v: %s",
				card, chip, flag, err, strings.TrimSpace(string(out)))
		}
		klog.V(4).Infof("device-share card=%s chip=%s set to %s", card, chip, flag)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestApplyDeviceShare|TestRunNpuSmi' -v`
Expected: PASS (5 tests: `TestApplyDeviceShare_Enable`, `_Disable`, `_FailFast`, `_NoChips`, `TestRunNpuSmi_AnswersDeviceShareConfirmation`).

- [ ] **Step 5: Commit**

```bash
git add internal/server/device_share.go internal/server/device_share_test.go
git commit -m "feat(server): add npu-smi device-share helpers"
```

---

## Task 2: Wire device-share into `Allocate()`

**Files:**
- Modify: `internal/server/server.go` (the `Allocate` method, currently lines ~279-343)
- Test: `internal/server/server_test.go` (add `TestAllocate_DeviceShare`, add `reflect` import)

- [ ] **Step 1: Write the failing test**

Add a `reflect` import to `internal/server/server_test.go`. Change the import block's first group from:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
```

to:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"
```

Then append this test to `internal/server/server_test.go`:

```go
// ============================================================================
// Allocate device-share tests
// ============================================================================

func TestAllocate_DeviceShare(t *testing.T) {
	const commonWord = "Ascend910"

	// uuid1 -> card0/chip1, uuid2 -> card1/chip0, uuid3 -> card0/chip1 (same
	// chip as uuid1, to exercise dedup).
	makePS := func() *PluginServer {
		return &PluginServer{
			commonWord:        commonWord,
			nodeName:          "test-node",
			toAllocDeviceAnno: "hami.io/Ascend910-devices-to-allocate",
			allocAnno:         "huawei.com/Ascend910",
			mgr: &FakeManager{
				GetDeviceByUUIDFunc: func(uuid string) *manager.Device {
					switch uuid {
					case "uuid1":
						return &manager.Device{UUID: "uuid1", PhyID: 3, CardID: 0, DeviceID: 1}
					case "uuid2":
						return &manager.Device{UUID: "uuid2", PhyID: 4, CardID: 1, DeviceID: 0}
					case "uuid3":
						return &manager.Device{UUID: "uuid3", PhyID: 5, CardID: 0, DeviceID: 1}
					default:
						return nil
					}
				},
			},
		}
	}

	// setupPod wires a fake clientset with a pod whose to-allocate annotation
	// lists one container per uuid. vnpuMode == "" means the annotation is
	// omitted (native template mode).
	setupPod := func(vnpuMode string, uuids ...string) CleanupFunc {
		c1 := setupInRequestDevices(commonWord)
		toAllocAnno := "hami.io/Ascend910-devices-to-allocate"
		allocAnno := "huawei.com/Ascend910"
		ctrDevs := make([]device.ContainerDevices, 0, len(uuids))
		rtInfo := make([]ascend.RuntimeInfo, 0, len(uuids))
		for _, u := range uuids {
			ctrDevs = append(ctrDevs, device.ContainerDevices{cd(u, commonWord, 1024, 4)})
			rtInfo = append(rtInfo, ascend.RuntimeInfo{UUID: u, Temp: "vir01"})
		}
		containerDevs := device.EncodePodSingleDevice(device.PodSingleDevice(ctrDevs))
		rtData, _ := json.Marshal(rtInfo)
		annos := map[string]string{
			toAllocAnno:              containerDevs,
			allocAnno:                string(rtData),
			util.BindTimeAnnotations: "2024-01-01T00:00:00Z",
			util.DeviceBindPhase:     util.DeviceBindAllocating,
		}
		if vnpuMode != "" {
			annos[VNPUModeAnnotation] = vnpuMode
		}
		_, _, c2 := setupAllocateEnv("test-node", "test-pod", "default", len(uuids), annos)
		return composeCleanup(c2, c1)
	}

	reqsFor := func(uuids ...string) *v1beta1.AllocateRequest {
		crs := make([]*v1beta1.ContainerAllocateRequest, 0, len(uuids))
		for _, u := range uuids {
			crs = append(crs, &v1beta1.ContainerAllocateRequest{DevicesIDs: []string{u + "-0"}})
		}
		return &v1beta1.AllocateRequest{ContainerRequests: crs}
	}

	// callSet normalizes captured npu-smi calls into a comparable set of
	// space-joined arg strings (chipSet iteration order is nondeterministic).
	callSet := func(calls [][]string) map[string]bool {
		s := make(map[string]bool, len(calls))
		for _, c := range calls {
			s[strings.Join(c, " ")] = true
		}
		return s
	}

	t.Run("HamiCoreFlipsDeviceShare", func(t *testing.T) {
		t.Cleanup(setupPod(VNPUModeHamiCore, "uuid1"))
		var calls [][]string
		withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			return nil, nil
		})
		ps := makePS()
		if _, err := ps.Allocate(context.Background(), reqsFor("uuid1")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := [][]string{{"set", "-t", "device-share", "-i", "0", "-c", "1", "-d", "1"}}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("npu-smi calls:\ngot  %v\nwant %v", calls, want)
		}
	})

	t.Run("MultiCardTwoChips", func(t *testing.T) {
		t.Cleanup(setupPod(VNPUModeHamiCore, "uuid1", "uuid2"))
		var calls [][]string
		withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			return nil, nil
		})
		ps := makePS()
		if _, err := ps.Allocate(context.Background(), reqsFor("uuid1", "uuid2")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(calls) != 2 {
			t.Fatalf("expected 2 calls, got %d: %v", len(calls), calls)
		}
		got := callSet(calls)
		for _, w := range []string{
			"set -t device-share -i 0 -c 1 -d 1",
			"set -t device-share -i 1 -c 0 -d 1",
		} {
			if !got[w] {
				t.Fatalf("missing expected call %q in %v", w, calls)
			}
		}
	})

	t.Run("MultiContainerDedupsChip", func(t *testing.T) {
		// uuid1 and uuid3 share card0/chip1 -> exactly one flip.
		t.Cleanup(setupPod(VNPUModeHamiCore, "uuid1", "uuid3"))
		var calls [][]string
		withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			return nil, nil
		})
		ps := makePS()
		if _, err := ps.Allocate(context.Background(), reqsFor("uuid1", "uuid3")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := [][]string{{"set", "-t", "device-share", "-i", "0", "-c", "1", "-d", "1"}}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("expected one deduped call, got %v", calls)
		}
	})

	t.Run("NonHamiCoreNoFlip", func(t *testing.T) {
		t.Cleanup(setupPod("", "uuid1"))
		called := false
		withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
			called = true
			return nil, nil
		})
		ps := makePS()
		if _, err := ps.Allocate(context.Background(), reqsFor("uuid1")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Fatal("npu-smi must not be called for non-hami-core pods")
		}
	})

	t.Run("FlipFailureFailsAllocate", func(t *testing.T) {
		t.Cleanup(setupPod(VNPUModeHamiCore, "uuid1"))
		withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
			return []byte("E80001 not allowed"), fmt.Errorf("exit status 1")
		})
		ps := makePS()
		_, err := ps.Allocate(context.Background(), reqsFor("uuid1"))
		if err == nil || !strings.Contains(err.Error(), "device-share") {
			t.Fatalf("expected device-share error, got: %v", err)
		}
	})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server/ -run TestAllocate_DeviceShare -v`
Expected: FAIL — `HamiCoreFlipsDeviceShare`/`MultiCardTwoChips`/`MultiContainerDedupsChip` get 0 npu-smi calls (flip not wired yet); `FlipFailureFailsAllocate` gets a nil error. `NonHamiCoreNoFlip` may already pass.

- [ ] **Step 3: Implement the Allocate wiring**

In `internal/server/server.go`, replace this block of `Allocate()`:

```go
	// kubelet may call Allocate multiple times for the same pod, each time with
	// a subset of containers. Use pop semantics to match each request with its
	// corresponding containerDevices.
	responses := v1beta1.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
		containerDevs, err := ps.popNextContainerDevices(podSingleDev)
		if err != nil {
			return nil, fmt.Errorf("get next container devices: %w", err)
		}
		klog.Infof("containerDevs: %+v", containerDevs)

		if len(containerDevs) != len(req.DevicesIDs) {
			return nil, fmt.Errorf("device number not matched: annotation has %d, request has %d", len(containerDevs), len(req.DevicesIDs))
		}

		resp, err := ps.buildContainerAllocateResponse(pod, containerDevs, rtInfoLookup)
		if err != nil {
			return nil, fmt.Errorf("build container allocate response: %w", err)
		}
		responses.ContainerResponses = append(responses.ContainerResponses, resp)
	}

	// Patch the annotation with the in-memory erased podSingleDev.
	if err := ps.patchErasedAnnotation(pod, podSingleDev); err != nil {
```

with:

```go
	// kubelet may call Allocate multiple times for the same pod, each time with
	// a subset of containers. Use pop semantics to match each request with its
	// corresponding containerDevices.
	responses := v1beta1.AllocateResponse{}
	vnpuMode := pod.Annotations[VNPUModeAnnotation]
	chipSet := map[chipKey]struct{}{}
	for _, req := range reqs.ContainerRequests {
		containerDevs, err := ps.popNextContainerDevices(podSingleDev)
		if err != nil {
			return nil, fmt.Errorf("get next container devices: %w", err)
		}
		klog.Infof("containerDevs: %+v", containerDevs)

		if len(containerDevs) != len(req.DevicesIDs) {
			return nil, fmt.Errorf("device number not matched: annotation has %d, request has %d", len(containerDevs), len(req.DevicesIDs))
		}

		// hami-core soft slicing requires npu-smi device-share enabled on every
		// assigned chip; collect the distinct chips for this call so we can flip
		// them once below.
		if vnpuMode == VNPUModeHamiCore {
			for _, dev := range containerDevs {
				d := ps.mgr.GetDeviceByUUID(dev.UUID)
				if d == nil {
					return nil, fmt.Errorf("device-share lookup: unknown uuid %s", dev.UUID)
				}
				chipSet[chipKey{Card: d.CardID, Chip: d.DeviceID}] = struct{}{}
			}
		}

		resp, err := ps.buildContainerAllocateResponse(pod, containerDevs, rtInfoLookup)
		if err != nil {
			return nil, fmt.Errorf("build container allocate response: %w", err)
		}
		responses.ContainerResponses = append(responses.ContainerResponses, resp)
	}

	// Flip device-share once per Allocate (deduped across containers) before
	// erasing the to-allocate annotation: on failure we return early with the
	// annotation intact, so a kubelet retry decodes the same state. Fail-fast —
	// success stays false, so the deferred handler releases the node lock and
	// marks bind-phase failed.
	if vnpuMode == VNPUModeHamiCore && len(chipSet) > 0 {
		chips := make([]chipKey, 0, len(chipSet))
		for c := range chipSet {
			chips = append(chips, c)
		}
		if err := applyDeviceShare(chips, true); err != nil {
			return nil, fmt.Errorf("apply device-share for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	// Patch the annotation with the in-memory erased podSingleDev.
	if err := ps.patchErasedAnnotation(pod, podSingleDev); err != nil {
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/server/ -run TestAllocate_DeviceShare -v`
Expected: PASS (5 subtests).

- [ ] **Step 5: Run the full server package test suite (no regressions)**

Run: `go test ./internal/server/...`
Expected: PASS (existing `TestAllocate`, `TestNewPluginServer`, gRPC restart tests, etc. still green).

- [ ] **Step 6: Commit**

```bash
git add internal/server/server.go internal/server/server_test.go
git commit -m "feat(server): auto-enable npu-smi device-share for hami-core pods"
```

---

## Task 3: Documentation

**Files:**
- Modify: `README.md`
- Modify: `README_cn.md`

- [ ] **Step 1: Update `README.md` device-share requirement**

In `README.md`, under `hami-vnpu-core Soft Slicing Requirements`, replace the `**Chip Mode**` bullet and the manual "Enabling `device-share` Mode" instructions with an auto-managed note. Replace:

```markdown
- **Chip Mode**: enable `device-share` mode on Ascend chips for virtualization
Below is the English translation of the instructions for enabling `device-share` mode:
```

with:

```markdown
- **Chip Mode**: `device-share` is **auto-managed**. When a Pod sets
  `huawei.com/vnpu-mode: hami-core`, the device plugin runs
  `npu-smi set -t device-share -i <card> -c <chip> -d 1` on each chip assigned
  to that Pod at allocation time. No manual `npu-smi` step is required.

  The plugin resolves `npu-smi` from, in order:
  `/usr/local/Ascend/driver/tools/npu-smi` (provided by the existing driver
  hostPath mount), `/usr/local/sbin/npu-smi`, `/usr/local/bin/npu-smi`, then
  `PATH`. If your host ships `npu-smi` elsewhere, add a single-file hostPath
  mount in `ascend-device-plugin.yaml`, e.g. `/usr/local/sbin/npu-smi`.

  If the flip fails, the Pod's allocation fails and the error appears in the
  Pod's events.

<details>
<summary>Legacy: manually enabling <code>device-share</code> (no longer required)</summary>
```

and add a closing `</details>` line immediately after the existing manual instructions table (after the `value` / `1: Enabled` table, before the `## Compile` heading).

- [ ] **Step 2: Update `README_cn.md`**

Apply the equivalent change in `README_cn.md`: state that `device-share` 由插件在 `hami-core` Pod 的 Allocate 阶段自动开启(`npu-smi set -t device-share -i <card> -c <chip> -d 1`),无需手动执行;说明 npu-smi 解析顺序与可选的 `/usr/local/sbin/npu-smi` 单文件挂载;翻转失败会让 Pod 分配失败并在事件中显示。把原手动开启说明折叠为「历史/已不需要」。

- [ ] **Step 3: Commit**

```bash
git add README.md README_cn.md
git commit -m "docs: device-share is auto-managed for hami-core pods"
```

---

## Self-Review

**Spec coverage:**
- §3 trigger (`hami-core` annotation only) → Task 2 gate `vnpuMode == VNPUModeHamiCore`. ✓
- §4.1 `device_share.go` (chipKey, candidates var, runNpuSmi var, resolveNpuSmi, applyDeviceShare; dropped label gate) → Task 1. ✓
- §4.2 Allocate integration (collect in loop, flip once before patch, fail-fast) → Task 2 Step 3. ✓
- §5 deployment (no default yaml change; rely on driver-tools path; document optional mount) → Task 3 README note. ✓
- §6 fail-fast → Task 2 `FlipFailureFailsAllocate` + early return. ✓
- §7 tests (applyDeviceShare happy/fail-fast/empty; resolveNpuSmi via candidate var; Allocate hami-core flips, non-hami-core zero, dedup) → Tasks 1 & 2. ✓
- §8 docs → Task 3. ✓
- **Open item resolution:** §10 candidate list = package `var` (confirmed by `TestRunNpuSmi_AnswersDeviceShareConfirmation` reassigning it); chip collection = inline in the Allocate loop (least diff). ✓

**Placeholder scan:** none — every step has complete code or an exact textual edit.

**Type consistency:** `chipKey{Card, Chip int32}`, `applyDeviceShare([]chipKey, bool) error`, `runNpuSmi(...string) ([]byte, error)`, `npuSmiCandidates []string`, `VNPUModeHamiCore`/`VNPUModeAnnotation` (existing consts), `manager.Device.CardID`/`DeviceID` (existing fields), `ps.mgr.GetDeviceByUUID` (existing `Manager` method) — all consistent across tasks. The deployment plumbing needs no `manager.Manager` interface change.
