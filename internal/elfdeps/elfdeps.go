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

// Package elfdeps statically checks that ELF shared objects inside a foreign
// rootfs would load: every DT_NEEDED resolves on the search path and every
// required symbol version (e.g. GLIBC_2.34) is defined by the library that
// provides it. It stands in for running the target's dynamic linker when the
// rootfs is for another architecture and cannot be executed at build time.
//
// It is deliberately stricter and simpler than ld.so: the caller supplies the
// full search path (DT_RPATH/DT_RUNPATH and ld.so.conf are not read), weak
// version requirements count as required, and the first regular file on the
// path wins without an ELF class/machine check. Keep rootfs layouts where
// those differences cannot matter.
package elfdeps

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Object is what Check needs to know about one shared object.
type Object struct {
	// Needed lists DT_NEEDED entries in order.
	Needed []string
	// Requires maps a needed file name to the symbol versions required from it.
	Requires map[string][]string
	// Provides lists the symbol versions this object defines.
	Provides []string
}

// Loader reads the Object at an absolute host path.
type Loader func(path string) (*Object, error)

// Checker resolves libraries inside Root the way the dynamic linker would.
type Checker struct {
	// Root is the host directory holding the target rootfs.
	Root string
	// SearchPath lists library directories as seen inside Root, in lookup order.
	SearchPath []string
	// Load reads an object; nil means LoadELF.
	Load Loader
}

// Check resolves each target and its transitive dependencies, returning one
// error per problem found. A target containing "/" is a path inside Root;
// otherwise it is a library name looked up on SearchPath.
func (c Checker) Check(targets ...string) []error {
	load := c.Load
	if load == nil {
		load = LoadELF
	}
	var errs []error
	seen := map[string]bool{}
	loaded := map[string]*Object{}
	var order []string
	type item struct{ name, neededBy string }
	queue := make([]item, 0, len(targets))
	for _, t := range targets {
		queue = append(queue, item{name: t})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.name] {
			continue
		}
		seen[it.name] = true

		host, ok := c.lookup(it.name)
		if !ok {
			msg := fmt.Sprintf("%s: not found on search path %s", it.name, strings.Join(c.SearchPath, ":"))
			if it.neededBy != "" {
				msg += " (needed by " + it.neededBy + ")"
			}
			errs = append(errs, errors.New(msg))
			continue
		}
		obj, err := load(host)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", it.name, err))
			continue
		}
		loaded[it.name] = obj
		order = append(order, it.name)
		for _, dep := range obj.Needed {
			queue = append(queue, item{name: dep, neededBy: it.name})
		}
	}

	// Versions are checked against the object each name resolved to; a
	// provider that failed to resolve has already been reported above.
	for _, name := range order {
		obj := loaded[name]
		for _, dep := range obj.Needed {
			provider, ok := loaded[dep]
			if !ok {
				continue
			}
			for _, ver := range obj.Requires[dep] {
				if !slices.Contains(provider.Provides, ver) {
					errs = append(errs, fmt.Errorf("%s: requires %s from %s, which does not define it", name, ver, dep))
				}
			}
		}
	}
	return errs
}

// lookup finds a library name on SearchPath (or takes a path as-is) and
// returns its host path.
func (c Checker) lookup(name string) (string, bool) {
	if strings.Contains(name, "/") {
		return c.resolve(name)
	}
	for _, dir := range c.SearchPath {
		if host, ok := c.resolve(path.Join(dir, name)); ok {
			return host, true
		}
	}
	return "", false
}

// maxSymlinks matches Linux's MAXSYMLINKS.
const maxSymlinks = 40

// resolve maps a path inside Root to an existing regular file on the host,
// following symlinks component by component so that absolute link targets
// stay inside Root instead of escaping to the host filesystem.
func (c Checker) resolve(p string) (string, bool) {
	cur := "/"
	rest := strings.Split(strings.TrimPrefix(path.Clean("/"+p), "/"), "/")
	for hops := 0; len(rest) > 0; {
		next := path.Join(cur, rest[0])
		rest = rest[1:]
		host := filepath.Join(c.Root, filepath.FromSlash(next))
		fi, err := os.Lstat(host)
		if err != nil {
			return "", false
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > maxSymlinks {
			return "", false
		}
		target, err := os.Readlink(host)
		if err != nil {
			return "", false
		}
		if !path.IsAbs(target) {
			target = path.Join(cur, target)
		}
		// Restart from Root with the link target spliced in front of what is left.
		rest = append(strings.Split(strings.TrimPrefix(path.Clean(target), "/"), "/"), rest...)
		cur = "/"
	}
	host := filepath.Join(c.Root, filepath.FromSlash(cur))
	if fi, err := os.Stat(host); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return host, true
}
