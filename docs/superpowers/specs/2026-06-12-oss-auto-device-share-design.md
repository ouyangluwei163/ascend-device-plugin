# Auto device-share Management (OSS) — Design Spec

- **Date**: 2026-06-12
- **Repo/Branch**: `Project-HAMi/ascend-device-plugin`, `main`
- **Author**: ouyangluwei
- **Status**: Design — pending implementation plan

## 1. Background

The commercial fork (`vast` `release-2605.v2.1.x`) added automatic
`npu-smi set -t device-share` management in `internal/server/device_share.go`:
at Allocate it flips `device-share` on each chip a Pod is assigned, so the
operator no longer has to enable it by hand. The commercial trigger is
two-gated — Pod label `rise.io/device-share=true` **AND** node label
`hami.io/ascend-mode=vcann-rt`, the latter written by vast's
`NodeConfigReconciler`.

The OSS `main` branch has **neither** the `vcann-rt` mode nor any controller
that writes `hami.io/ascend-mode`, so a verbatim copy would carry a node-side
gate that nothing ever sets. We therefore re-base the feature on a signal the
OSS plugin already computes.

Today the OSS README lists, as a manual prerequisite for `hami-vnpu-core`
soft slicing:

> **Chip Mode**: enable `device-share` mode on Ascend chips for virtualization
> (`npu-smi set -t device-share -i <id> -d 1`)

`device-share` is, in effect, a precondition of the existing `hami-core`
soft-slicing path. This design makes the plugin perform that flip
automatically whenever a Pod opts into `hami-core`.

The fork point of the two branches is `ce4c6ea` (PR #61, the commit that
integrated `hami-vnpu-core`); `device_share.go` was added on the commercial
side after that point.

## 2. Goals / Non-Goals

**Goals**
- Auto-enable `device-share` on a Pod's assigned chips at Allocate time, with
  **no manual `npu-smi` step and no new Pod/Node labels**.
- Trigger purely on the existing `huawei.com/vnpu-mode: hami-core` annotation.
- Fail fast: if the flip fails, the Allocate fails and kubelet surfaces the
  error on the Pod.
- Fit the OSS multi-container Allocate flow (per-container responses, pop
  semantics) — flip once per Allocate over the distinct chips in that call.

**Non-Goals**
- No `-d 0` (disable) path. The plugin only ever writes `-d 1`. Fresh chips
  start disabled, and the scheduler is responsible for not co-locating
  share and non-share workloads on the same chip. (Matches commercial.)
- No reference counting / GC of device-share state across Pods.
- No node-side gate (`hami.io/ascend-mode`) and no `rise.io/device-share` Pod
  label — both dropped relative to the commercial version.
- No change to the `manager.Manager` interface — `Device.CardID` /
  `Device.DeviceID` are already exported and reachable via the existing
  `GetDeviceByUUID`.
- No `vcann-rt` mode (that is a separate, larger port).

## 3. Trigger and gating

| Signal | Source | Role |
|---|---|---|
| `huawei.com/vnpu-mode` == `hami-core` | Pod annotation (already read in `buildContainerAllocateResponse`) | **Sole gate.** Any other value (incl. unset = native template mode) → no npu-smi call. |

No node label, no Pod label, no k8s client node GET. This removes the
commercial `nodeWantsDeviceShare()`, `wantsDeviceShare()`,
`PodLabelDeviceShare`, `NodeLabelAscendMode`, `NodeAscendModeVCannRT`, and the
`client.GetClient().CoreV1().Nodes().Get(...)` round trip.

## 4. Code structure

### 4.1 New file `internal/server/device_share.go`

Ported and slimmed from the commercial file. Exports nothing; consumed only by
`server.go`.

```go
// chipKey identifies one NPU chip by the two coordinates npu-smi takes for its
// -i (card) and -c (chip) flags.
type chipKey struct {
    Card int32
    Chip int32
}

// npuSmiCandidates lists host paths where npu-smi may live, priority order.
// The first is what the daemonset's /usr/local/Ascend/driver hostPath exposes.
var npuSmiCandidates = []string{
    "/usr/local/Ascend/driver/tools/npu-smi",
    "/usr/local/sbin/npu-smi",
    "/usr/local/bin/npu-smi",
}

// runNpuSmi executes npu-smi and returns combined output. Package var so tests
// substitute a fake. Enabling device-share (-d 1) prints a "There are security
// risks ... continue?(Y/N)" prompt and aborts (exit 200) if stdin is closed;
// npu-smi has no -y flag in targeted driver versions, so we feed "Y\n"
// unconditionally — commands that don't prompt ignore the unread stdin.
var runNpuSmi = func(args ...string) ([]byte, error) { ... }

func resolveNpuSmi() (string, error)            // stat candidates, then exec.LookPath
func applyDeviceShare(chips []chipKey, enabled bool) error
```

`applyDeviceShare`:
- `len(chips) == 0` → return nil (no-op).
- For each chip: `npu-smi set -t device-share -i <card> -c <chip> -d <flag>`
  where `flag = "1"` when `enabled` (always, in this design), else `"0"`.
- Fail fast on the first per-chip error, wrapping the trimmed combined output.
- Log each success at `klog.V(4)`.

**Dropped from the commercial file:** `wantsDeviceShare`,
`nodeWantsDeviceShare`, `PodLabelDeviceShare`, `NodeLabelAscendMode`,
`NodeAscendModeVCannRT`, and the metav1/client imports they required.

### 4.2 `internal/server/server.go` — `Allocate()` integration (approach A)

Accumulate the distinct chips for the containers allocated in **this** call,
then flip once, **before** `patchErasedAnnotation`. Pseudocode (additions
marked `+`):

```go
  responses := v1beta1.AllocateResponse{}
+ vnpuMode := pod.Annotations[VNPUModeAnnotation]
+ chipSet := map[chipKey]struct{}{}
  for _, req := range reqs.ContainerRequests {
      containerDevs, err := ps.popNextContainerDevices(podSingleDev)
      ...
+     if vnpuMode == VNPUModeHamiCore {
+         for _, dev := range containerDevs {
+             d := ps.mgr.GetDeviceByUUID(dev.UUID)
+             if d == nil {
+                 return nil, fmt.Errorf("device-share lookup: unknown uuid %s", dev.UUID)
+             }
+             chipSet[chipKey{Card: d.CardID, Chip: d.DeviceID}] = struct{}{}
+         }
+     }
      resp, err := ps.buildContainerAllocateResponse(pod, containerDevs, rtInfoLookup)
      ...
      responses.ContainerResponses = append(responses.ContainerResponses, resp)
  }

+ if vnpuMode == VNPUModeHamiCore && len(chipSet) > 0 {
+     chips := make([]chipKey, 0, len(chipSet))
+     for c := range chipSet {
+         chips = append(chips, c)
+     }
+     if err := applyDeviceShare(chips, true); err != nil {
+         return nil, fmt.Errorf("apply device-share for pod %s/%s: %w",
+             pod.Namespace, pod.Name, err)
+     }
+ }

  if err := ps.patchErasedAnnotation(pod, podSingleDev); err != nil { ... }
  success = true
  return &responses, nil
```

Notes:
- The device lookup duplicates the one inside `buildContainerAllocateResponse`;
  this is an in-memory slice scan and is negligible. Keeping it here avoids
  changing the builder's signature (it stays single-purpose: build one
  container response).
- Ordering: flip happens **after** the loop but **before**
  `patchErasedAnnotation`. On flip failure we return early, so the
  to-allocate annotation is left intact and a kubelet retry decodes the same
  state. `success` stays false, so the deferred handler calls
  `podAllocationFailed` (releases the node lock, sets bind-phase failed).
- Iteration order over `chipSet` is nondeterministic; tests must assert on the
  **set** of npu-smi calls, not their order.

## 5. npu-smi resolution and deployment

The plugin DaemonSet already mounts the host driver tree at
`/usr/local/Ascend/driver` (volume `hiai-driver`), so
`/usr/local/Ascend/driver/tools/npu-smi` — the first candidate — resolves with
**no deployment change required**.

`ascend-device-plugin.yaml`: no change by default. Document the optional
single-file fallback for hosts that ship npu-smi elsewhere
(e.g. `/usr/local/sbin/npu-smi`):

```yaml
# optional, only if npu-smi is not under the mounted driver tree
volumeMounts:
  - name: host-npu-smi
    mountPath: /usr/local/sbin/npu-smi
volumes:
  - name: host-npu-smi
    hostPath:
      path: /usr/local/sbin/npu-smi
```

## 6. Failure handling

Fail-fast (per decision): any `applyDeviceShare` error → `Allocate` returns the
wrapped error; kubelet shows it in Pod events. Rationale: if `device-share`
cannot be enabled, `hami-core` soft slicing would be broken anyway, so a loud
failure beats silent misbehavior. Blast-radius caveat: a node whose npu-smi is
unreachable at all candidate paths will fail **every** `hami-core` Pod on that
node until fixed — acceptable and diagnosable via the Pod event.

## 7. Testing

**`internal/server/device_share_test.go` (new)**
- `applyDeviceShare`:
  - happy path — N chips → N calls, each with correct `-i/-c/-d 1` args
    (assert via fake `runNpuSmi` capturing args).
  - mid-list failure — second chip errors → returns wrapped error, stops
    (fail-fast); assert no further calls.
  - empty chips → nil, zero calls.
- `resolveNpuSmi`: candidate ordering (first existing+executable wins);
  not-found → error (use a temp dir + overridden candidate list, or document
  as integration-only if candidates are package-const).

**`internal/server/server_test.go` (extend)**
- hami-core Pod (annotation set) with a known device → exactly one
  `applyDeviceShare` invocation covering the right `chipKey`s; assert on the
  **set** of calls.
- non-hami-core Pod (template/default, annotation unset) → **zero** npu-smi
  calls.
- multi-container / multi-device same-chip → chip appears once (dedup).
- flip failure → `Allocate` returns error and `podAllocationFailed` path runs
  (lock released, annotation not erased).

Test seams already present: `runNpuSmi` is a package var; `PluginServer` has
`dialFunc` / `registerKubeletFunc` / `prepareHostResourcesFunc` hooks and the
`manager.Manager` interface (with `fake_manager_test.go`) for injecting
devices.

## 8. Documentation

`README.md` / `README_cn.md`: in the `hami-vnpu-core` requirements, change the
manual `device-share` step to "auto-managed per `hami-core` Pod by the plugin;
manual enablement is no longer required." Note the npu-smi resolution paths and
the optional single-file mount.

## 9. Decisions log

- **Trigger = `hami-core` annotation** (not the commercial dual label). The OSS
  branch lacks `vcann-rt` and the `NodeConfigReconciler`; `device-share` is the
  natural precondition of `hami-core`, so the existing annotation is the
  cleanest gate.
- **Write-only (`-d 1`)** — same as commercial; scheduler owns co-location
  safety.
- **Fail-fast** on npu-smi error.
- **Approach A** (flip in `Allocate`, new `device_share.go`) over flipping
  inside `buildContainerAllocateResponse` (B, per-container/redundant) or a
  faithful dual-gate copy (C, dead node gate).

## 10. Open items for the implementation plan

- Exact helper shape for chip collection (inline loop vs. a
  `collectChips(containerDevs)` method) — least-diff preference: inline.
- Whether `resolveNpuSmi` candidate list should be a package var (to allow a
  unit test to point it at a temp dir) vs. const + integration-only coverage.
- Final README wording in both languages.
