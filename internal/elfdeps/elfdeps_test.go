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

package elfdeps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sysroot builds a fake rootfs: each key is a root-relative file path whose
// content is irrelevant; the fake loader answers by that same path.
type sysroot struct {
	t       *testing.T
	dir     string
	objects map[string]*Object
}

func newSysroot(t *testing.T) *sysroot {
	return &sysroot{t: t, dir: t.TempDir(), objects: map[string]*Object{}}
}

func (s *sysroot) add(path string, obj *Object) {
	s.t.Helper()
	full := filepath.Join(s.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, nil, 0o644); err != nil {
		s.t.Fatal(err)
	}
	s.objects[path] = obj
}

func (s *sysroot) link(path, target string) {
	s.t.Helper()
	full := filepath.Join(s.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sysroot) checker(searchPath ...string) Checker {
	return Checker{
		Root:       s.dir,
		SearchPath: searchPath,
		Load: func(path string) (*Object, error) {
			rel, err := filepath.Rel(s.dir, path)
			if err != nil {
				return nil, err
			}
			obj, ok := s.objects["/"+filepath.ToSlash(rel)]
			if !ok {
				return nil, fmt.Errorf("fake loader: no object at %s", rel)
			}
			return obj, nil
		},
	}
}

func assertProblems(t *testing.T, errs []error, wantSubstrings ...string) {
	t.Helper()
	if len(errs) != len(wantSubstrings) {
		t.Fatalf("got %d problems %v, want %d matching %q", len(errs), errs, len(wantSubstrings), wantSubstrings)
	}
	for i, want := range wantSubstrings {
		if !strings.Contains(errs[i].Error(), want) {
			t.Errorf("problem %d = %q, want it to mention %q", i, errs[i], want)
		}
	}
}

func TestCheck_LibraryNotOnSearchPath(t *testing.T) {
	root := newSysroot(t)
	root.add("/opt/elsewhere/liburma.so.0", &Object{})

	errs := root.checker("/usr/lib64").Check("liburma.so.0")

	assertProblems(t, errs, "liburma.so.0")
}

func TestCheck_TransitiveDependencyMissing(t *testing.T) {
	root := newSysroot(t)
	root.add("/usr/lib64/liburma.so.0", &Object{Needed: []string{"liburma_common.so.0", "libc.so.6"}})
	root.add("/usr/lib/aarch64-linux-gnu/libc.so.6", &Object{})

	errs := root.checker("/usr/lib64", "/usr/lib/aarch64-linux-gnu").Check("liburma.so.0")

	assertProblems(t, errs, "liburma_common.so.0")
}

func TestCheck_RequiredSymbolVersionMissing(t *testing.T) {
	root := newSysroot(t)
	root.add("/usr/lib64/liburma.so.0", &Object{
		Needed:   []string{"libc.so.6"},
		Requires: map[string][]string{"libc.so.6": {"GLIBC_2.17", "GLIBC_2.34"}},
	})
	root.add("/lib/libc.so.6", &Object{Provides: []string{"libc.so.6", "GLIBC_2.17", "GLIBC_2.31"}})

	errs := root.checker("/usr/lib64", "/lib").Check("liburma.so.0")

	assertProblems(t, errs, "GLIBC_2.34")
}

func TestCheck_AllSatisfied(t *testing.T) {
	root := newSysroot(t)
	root.add("/usr/lib64/liburma.so.0", &Object{
		Needed:   []string{"liburma_common.so.0", "libc.so.6"},
		Requires: map[string][]string{"libc.so.6": {"GLIBC_2.34"}},
	})
	root.add("/usr/lib64/liburma_common.so.0", &Object{Needed: []string{"libc.so.6"}})
	root.add("/lib/libc.so.6", &Object{Provides: []string{"GLIBC_2.34"}})

	errs := root.checker("/usr/lib64", "/lib").Check("liburma.so.0")

	assertProblems(t, errs)
}

func TestCheck_AbsoluteSymlinkResolvesInsideRoot(t *testing.T) {
	root := newSysroot(t)
	root.add("/usr/lib64/liburma.so.0.0.3", &Object{})
	root.link("/usr/lib64/liburma.so.0", "/usr/lib64/liburma.so.0.0.3")

	errs := root.checker("/usr/lib64").Check("liburma.so.0")

	assertProblems(t, errs)
}

func TestCheck_TargetGivenAsPath(t *testing.T) {
	root := newSysroot(t)
	root.add("/usr/lib64/urma/liburma-udma.so", &Object{Needed: []string{"libummu.so.1"}})

	errs := root.checker("/usr/lib64").Check("/usr/lib64/urma/liburma-udma.so")

	assertProblems(t, errs, "libummu.so.1")
}
