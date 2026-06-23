# Node-level device-share Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable `device-share` on every chip of the node once at device-plugin startup (gated on `IsHamiVnpuCore()`), instead of per-Pod at `Allocate`.

**Architecture:** Add a `(*PluginServer).enableNodeDeviceShare()` method that, when the node is hami-vnpu-core, flips device-share on all chips returned by the manager; call it from `Start()` after `UpdateDevice()`. Remove the per-Pod device-share logic from `Allocate()`. Reuse the existing `chipKey` type and `applyDeviceShare` helper.

**Tech Stack:** Go, `k8s.io/klog/v2`, `npu-smi` (via the existing `runNpuSmi`/`applyDeviceShare` package vars in `internal/server/device_share.go`).

## Global Constraints

- Enable device-share **only** when `ps.mgr.IsHamiVnpuCore() == true`. When false, do nothing — never write `-d 0`.
- **Fail-fast:** any per-chip flip failure must propagate out of `Start()` so the process exits (`main.go` `klog.Fatalf`) and kubelet restarts it.
- **Idempotent:** the flip runs on every `Start()` (process start and kubelet-socket/SIGHUP restart); `npu-smi set` accepts redundant writes.
- npu-smi invocation, resolution, and the `chipKey{Card, Chip int32}` type already exist in `internal/server/device_share.go` — reuse them, do not duplicate.
- Package under change is `internal/server` (Go package `server`). The `FakeManager` test double lives in `internal/server/fake_manager_test.go` and already exposes `IsHamiVnpuCoreFunc`, `GetDevicesFunc`, `GetDeviceByUUIDFunc`, `UpdateDeviceFunc`.

---

### Task 1: Node-level device-share at startup

**Files:**
- Modify: `internal/server/device_share.go` (add `enableNodeDeviceShare` method)
- Modify: `internal/server/server.go:122` (call it from `Start()` after `UpdateDevice()`)
- Test: `internal/server/device_share_test.go` (add 3 tests + `manager` import)

**Interfaces:**
- Consumes:
  - `ps.mgr.IsHamiVnpuCore() bool`
  - `ps.mgr.GetDevices() []*manager.Device` where `manager.Device` has fields `CardID int32`, `DeviceID int32`
  - `applyDeviceShare(chips []chipKey, enabled bool) error` (existing, in `device_share.go`)
  - `chipKey{Card int32, Chip int32}` (existing, in `device_share.go`)
- Produces:
  - `func (ps *PluginServer) enableNodeDeviceShare() error`

- [ ] **Step 1: Write the failing tests**

Add the `manager` import to the import block of `internal/server/device_share_test.go` (it currently imports `fmt`, `os`, `path/filepath`, `reflect`, `strings`, `testing`):

```go
	"github.com/Project-HAMi/ascend-device-plugin/internal/manager"
```

Append these three tests to `internal/server/device_share_test.go`:

```go
func TestEnableNodeDeviceShare_NotHamiVnpuCore(t *testing.T) {
	called := false
	withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	ps := &PluginServer{
		nodeName: "node-1",
		mgr: &FakeManager{
			IsHamiVnpuCoreFunc: func() bool { return false },
			GetDevicesFunc: func() []*manager.Device {
				return []*manager.Device{{CardID: 0, DeviceID: 0}}
			},
		},
	}
	if err := ps.enableNodeDeviceShare(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("npu-smi must not be invoked when node is not hami-vnpu-core")
	}
}

func TestEnableNodeDeviceShare_FlipsAllChipsDeduped(t *testing.T) {
	type ic struct{ card, chip string }
	seen := map[ic]int{}
	withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
		// args: set -t device-share -i <card> -c <chip> -d <flag>
		if len(args) != 9 || args[8] != "1" {
			t.Errorf("unexpected npu-smi args: %v", args)
			return nil, nil
		}
		seen[ic{args[4], args[6]}]++
		return nil, nil
	})
	ps := &PluginServer{
		nodeName: "node-1",
		mgr: &FakeManager{
			IsHamiVnpuCoreFunc: func() bool { return true },
			GetDevicesFunc: func() []*manager.Device {
				return []*manager.Device{
					{CardID: 0, DeviceID: 0},
					{CardID: 0, DeviceID: 1},
					{CardID: 0, DeviceID: 0}, // duplicate chip — must be flipped once
					{CardID: 1, DeviceID: 0},
				}
			},
		},
	}
	if err := ps.enableNodeDeviceShare(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[ic]int{
		{"0", "0"}: 1,
		{"0", "1"}: 1,
		{"1", "0"}: 1,
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("device-share calls mismatch:\ngot  %v\nwant %v", seen, want)
	}
}

func TestEnableNodeDeviceShare_FlipFailureFailsFast(t *testing.T) {
	withFakeNpuSmi(t, func(args ...string) ([]byte, error) {
		return []byte("E80001 not allowed"), fmt.Errorf("exit status 1")
	})
	ps := &PluginServer{
		nodeName: "node-1",
		mgr: &FakeManager{
			IsHamiVnpuCoreFunc: func() bool { return true },
			GetDevicesFunc: func() []*manager.Device {
				return []*manager.Device{{CardID: 0, DeviceID: 0}}
			},
		},
	}
	if err := ps.enableNodeDeviceShare(); err == nil {
		t.Fatal("expected error when npu-smi flip fails, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run TestEnableNodeDeviceShare -v`
Expected: FAIL — compile error `ps.enableNodeDeviceShare undefined (type *PluginServer has no field or method enableNodeDeviceShare)`.

- [ ] **Step 3: Implement `enableNodeDeviceShare`**

Append to `internal/server/device_share.go` (the file already imports `fmt` and `k8s.io/klog/v2`; no new imports needed):

```go
// enableNodeDeviceShare turns device-share on for every chip on the node when
// the node is configured for hami-vnpu-core soft slicing. It is called once at
// startup (from Start, after UpdateDevice has populated the device list) and is
// idempotent: npu-smi accepts redundant set commands, so a plugin restart
// simply re-applies the same state.
//
// When the node is not hami-vnpu-core it is a no-op — it never writes -d 0, so
// share state set for other purposes is left untouched.
//
// Fail-fast: any per-chip failure is returned to the caller, which aborts
// startup so kubelet restarts the plugin and retries.
func (ps *PluginServer) enableNodeDeviceShare() error {
	if !ps.mgr.IsHamiVnpuCore() {
		klog.V(3).Infof("node %s is not hami-vnpu-core, skipping device-share", ps.nodeName)
		return nil
	}
	chipSet := map[chipKey]struct{}{}
	for _, d := range ps.mgr.GetDevices() {
		chipSet[chipKey{Card: d.CardID, Chip: d.DeviceID}] = struct{}{}
	}
	if len(chipSet) == 0 {
		klog.Warningf("node %s is hami-vnpu-core but no devices found for device-share", ps.nodeName)
		return nil
	}
	chips := make([]chipKey, 0, len(chipSet))
	for c := range chipSet {
		chips = append(chips, c)
	}
	if err := applyDeviceShare(chips, true); err != nil {
		return fmt.Errorf("enable node device-share: %w", err)
	}
	klog.Infof("device-share enabled on %d chip(s) of node %s", len(chips), ps.nodeName)
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/server/ -run TestEnableNodeDeviceShare -v`
Expected: PASS (all three).

- [ ] **Step 5: Wire it into `Start()`**

In `internal/server/server.go`, find this block inside `Start()` (around line 122):

```go
	err := ps.mgr.UpdateDevice()
	if err != nil {
		return err
	}
	err = ps.serve()
```

Replace it with:

```go
	err := ps.mgr.UpdateDevice()
	if err != nil {
		return err
	}
	if err := ps.enableNodeDeviceShare(); err != nil {
		return err
	}
	err = ps.serve()
```

- [ ] **Step 6: Build and run the full server package tests**

Run: `go build ./... && go test ./internal/server/...`
Expected: build succeeds; all tests PASS (the existing `TestAllocate_DeviceShare` still passes here — it is removed in Task 2).

- [ ] **Step 7: Commit**

```bash
git add internal/server/device_share.go internal/server/device_share_test.go internal/server/server.go
git commit -m "feat(server): enable device-share at node level on startup

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Remove per-Pod device-share from Allocate

**Files:**
- Modify: `internal/server/server.go` (`Allocate`, around lines 315–362)
- Test: `internal/server/server_test.go` (remove `TestAllocate_DeviceShare`, lines 953–end)

**Interfaces:**
- Consumes: nothing new.
- Produces: nothing new. `chipKey` and `applyDeviceShare` remain referenced by `enableNodeDeviceShare` (Task 1); the constants `VNPUModeAnnotation` / `VNPUModeHamiCore` remain referenced by `internal/server/allocate.go`.

- [ ] **Step 1: Remove the `vnpuMode`/`chipSet` locals**

In `internal/server/server.go` `Allocate`, replace:

```go
	responses := v1beta1.AllocateResponse{}
	vnpuMode := pod.Annotations[VNPUModeAnnotation]
	chipSet := map[chipKey]struct{}{}
	for _, req := range reqs.ContainerRequests {
```

with:

```go
	responses := v1beta1.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
```

- [ ] **Step 2: Remove the in-loop chip collection**

Delete this block (including its leading blank line) from inside the `for _, req := range reqs.ContainerRequests` loop:

```go

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
```

- [ ] **Step 3: Remove the post-loop flip**

Delete this block (including its leading blank line), which sits between the request loop and the `// Patch the annotation ...` comment:

```go

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
```

After these three edits, the loop body ends with `responses.ContainerResponses = append(...)` and is immediately followed by the `// Patch the annotation with the in-memory erased podSingleDev.` comment.

- [ ] **Step 4: Remove the obsolete Allocate device-share test**

In `internal/server/server_test.go`, delete the entire `func TestAllocate_DeviceShare(t *testing.T) { ... }` (starts at line 953 and runs to the end of the file, including its local `setupPod` closure). It is the last function in the file.

- [ ] **Step 5: Build and run the full server package tests**

Run: `go build ./... && go test ./internal/server/...`
Expected: build succeeds (no `declared and not used` / undefined-symbol errors); all remaining tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/server.go internal/server/server_test.go
git commit -m "refactor(server): drop per-Pod device-share flip from Allocate

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Update documentation

**Files:**
- Modify: `README.md` (lines 27–39, the `**Chip Mode**` bullet)
- Modify: `README_cn.md` (lines 25–29, the 芯片模式 bullet)

**Interfaces:** none (docs only).

- [ ] **Step 1: Update `README.md`**

Replace this block (lines 27–39):

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
```

with:

```markdown
- **Chip Mode**: `device-share` is **auto-managed at the node level**. When the
  node is configured for hami-vnpu-core soft slicing (the `hami-vnpu-core` field
  in `device-node-config`), the device plugin enables device-share on **every
  chip on the node at startup**, running
  `npu-smi set -t device-share -i <card> -c <chip> -d 1` once per chip. No manual
  `npu-smi` step and no per-Pod annotation are required to enable it.

  The plugin resolves `npu-smi` from, in order:
  `/usr/local/Ascend/driver/tools/npu-smi` (provided by the existing driver
  hostPath mount), `/usr/local/sbin/npu-smi`, `/usr/local/bin/npu-smi`, then
  `PATH`. If your host ships `npu-smi` elsewhere, add a single-file hostPath
  mount in `ascend-device-plugin.yaml`, e.g. `/usr/local/sbin/npu-smi`.

  This config is read once at startup, so **changing `hami-vnpu-core` requires
  restarting the plugin** to take effect. If any chip's flip fails, the **plugin
  fails to start** and advertises no devices on the node until it succeeds.
```

- [ ] **Step 2: Update `README_cn.md`**

Replace this block (lines 25–29):

```markdown
- 芯片模式：`device-share` 现在由插件**自动管理**。当 Pod 设置 `huawei.com/vnpu-mode: hami-core` 时，插件在分配阶段对该 Pod 占用的每个芯片执行 `npu-smi set -t device-share -i <card> -c <chip> -d 1`，无需手动执行 `npu-smi`。

  插件按以下顺序解析 `npu-smi`：`/usr/local/Ascend/driver/tools/npu-smi`（由已有的 driver hostPath 挂载提供）、`/usr/local/sbin/npu-smi`、`/usr/local/bin/npu-smi`，最后是 `PATH`。若宿主机的 `npu-smi` 在其他位置，可在 `ascend-device-plugin.yaml` 中增加单文件 hostPath 挂载（例如 `/usr/local/sbin/npu-smi`）。

  若翻转失败，该 Pod 的分配会失败，错误会显示在 Pod 的事件中。
```

with:

```markdown
- 芯片模式：`device-share` 现在由插件在**节点级自动管理**。当节点被配置为 hami-vnpu-core 软切分（`device-node-config` 中的 `hami-vnpu-core` 字段）时，插件在**启动阶段对节点上的每块芯片**开启 device-share，逐芯片执行一次 `npu-smi set -t device-share -i <card> -c <chip> -d 1`，无需手动执行 `npu-smi`，也无需在 Pod 上加注解来开启。

  插件按以下顺序解析 `npu-smi`：`/usr/local/Ascend/driver/tools/npu-smi`（由已有的 driver hostPath 挂载提供）、`/usr/local/sbin/npu-smi`、`/usr/local/bin/npu-smi`，最后是 `PATH`。若宿主机的 `npu-smi` 在其他位置，可在 `ascend-device-plugin.yaml` 中增加单文件 hostPath 挂载（例如 `/usr/local/sbin/npu-smi`）。

  该配置仅在启动时读取一次，因此**修改 `hami-vnpu-core` 需重启插件**才能生效。若任一芯片翻转失败，**插件将启动失败**，在成功之前不会上报该节点的设备。
```

- [ ] **Step 3: Commit**

```bash
git add README.md README_cn.md
git commit -m "docs: device-share is node-level, enabled at startup

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```
