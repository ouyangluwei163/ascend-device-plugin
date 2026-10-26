# Submodule patches

`mind-cluster` is pinned to the upstream v26.1.1 commit. Run `make patch-submodules`
before invoking Go directly; `make all`, `make test`, and `make lint` apply the
patch automatically. Repeated application is safe. The submodule will show local
changes after patching; commit the patch file and gitlink, not the patched files
inside the submodule.

`0001-dcmi-multi-die-policy-clang.patch` makes the C multi-die policy wrappers use
`enum dcmi_multi_die_policy`, matching their header declarations and Go callers.
The upstream `unsigned int` definitions fail to compile with Clang. Remove this
patch when upgrading to an upstream revision that fixes the signatures.
