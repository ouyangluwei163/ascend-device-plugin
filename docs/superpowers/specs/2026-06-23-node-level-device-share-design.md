# Design: Node-level device-share enablement

Date: 2026-06-23
Status: Approved

## Problem

Today `device-share` is flipped per-Pod at `Allocate` time, gated on the Pod
annotation `huawei.com/vnpu-mode == hami-core` (see `internal/server/server.go`
`Allocate`). This couples a node-wide hardware setting to per-Pod scheduling
decisions: every hami-core Allocate shells out to `npu-smi`, and the chips a
node will ever soft-slice are not made share-capable until the first Pod lands
on them.

The node already declares whether it does hami-vnpu-core soft slicing — via the
`device-node-config` field `hami-vnpu-core`, surfaced as
`ps.mgr.IsHamiVnpuCore()` and reported to the node annotation `hami-vnpu-core`.
device-share is a node-level capability and should be driven from that
node-level signal, once, at startup.

## Behavior change

- **Before:** at `Allocate`, for each `huawei.com/vnpu-mode: hami-core` Pod,
  flip device-share on the chips that Pod was assigned.
- **After:** at device-plugin **startup**, flip device-share **on** for **every
  chip on the node**, gated on `ps.mgr.IsHamiVnpuCore() == true`. The `Allocate`
  path no longer touches device-share.

When `IsHamiVnpuCore() == false` the plugin does nothing — it never writes
`-d 0` (consistent with the "never actively disable" policy; avoids disturbing
share state set for other purposes).

## Placement and control flow (`internal/server/`)

`Start()` already calls `ps.mgr.UpdateDevice()` (server.go:122), which populates
the device list — each `Device` carries `CardID`/`DeviceID`, exactly the
`npu-smi -i <card> -c <chip>` coordinates. Insert the flip after
`UpdateDevice()` and before `serve()`:

```go
err := ps.mgr.UpdateDevice()
if err != nil { return err }
if err := ps.enableNodeDeviceShare(); err != nil {
    return err          // fail-fast: bubbles to main's klog.Fatalf
}
```

New method, in `device_share.go` (co-located with existing device-share code):

```go
func (ps *PluginServer) enableNodeDeviceShare() error {
    if !ps.mgr.IsHamiVnpuCore() {        // false branch: no-op, never -d 0
        klog.V(3).Infof("node %s not hami-vnpu-core, skip device-share", ps.nodeName)
        return nil
    }
    chipSet := map[chipKey]struct{}{}    // dedup by (CardID, DeviceID)
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

Reuses the existing `chipKey` type and `applyDeviceShare` (already fail-fast and
idempotent).

## Key properties

- **Idempotent / restart-safe.** `Start()` runs on process start and again on
  kubelet-socket recreation / SIGHUP restart; each pass re-applies device-share.
  `npu-smi set` accepts redundant writes, so re-application is harmless. This is
  the "restart supports idempotency" requirement.
- **Fail-fast.** Any per-chip failure → `enableNodeDeviceShare` returns err →
  `Start()` returns err → `start()` returns err → `main.go` `klog.Fatalf`, the
  process exits and kubelet restarts it. While it is failing, the node advertises
  no devices.
  - **Accepted trade-off:** a single bad chip takes the whole node's NPU
    advertisement offline (crashloop until the flip succeeds).
- **Enumeration scope.** `UpdateDevice()` is all-or-nothing (it returns an error
  on the first chip it cannot query, so `Start()` fails before reaching the flip).
  Therefore `GetDevices()` returns exactly the node's full set of healthy,
  addressable physical chips.

## Removals

Remove the device-share block from `server.go` `Allocate()`:

- the local `vnpuMode := pod.Annotations[VNPUModeAnnotation]` used for the flip,
- the `chipSet` map,
- the per-container `GetDeviceByUUID` collection loop,
- the post-loop `applyDeviceShare(chips, true)` call.

`allocate.go` `buildContainerAllocateResponse` (hami-core mounts/envs) is
**unchanged** — it reads `vnpuMode` independently for the response build.

## Testing

- **Keep:** `device_share_test.go` tests for `applyDeviceShare` /
  `resolveNpuSmi` / `runNpuSmi` (functions unchanged).
- **Remove:** the `server_test.go` regression test asserting that a failed
  device-share flip leaves the to-allocate annotation intact — that Allocate-time
  behavior no longer exists.
- **Add** (using the existing fake manager, extended with `IsHamiVnpuCore` /
  `GetDevices` controls):
  - `IsHamiVnpuCore() == false` → `npu-smi` is never invoked.
  - `IsHamiVnpuCore() == true` → `applyDeviceShare(.., true)` is called for every
    deduped chip.
  - flip failure → `enableNodeDeviceShare` returns an error (fail-fast).

## Documentation

Update `README.md` / `README_cn.md`:

- device-share is now **node-level, enabled at startup**, driven by the
  `device-node-config` `hami-vnpu-core` field (no longer per-Pod at allocation).
- changing that config requires **restarting the plugin** to take effect (the
  config is read once at process start; there is no live reload).
- if any chip's flip fails, the **plugin fails to start** (the node advertises no
  devices until it succeeds).
