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
	"debug/elf"
	"fmt"
)

// LoadELF reads DT_NEEDED and GNU symbol-version information from an ELF file.
// Objects without a version table (no .gnu.version) simply yield no versions.
func LoadELF(path string) (*Object, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	needed, err := f.ImportedLibraries()
	if err != nil {
		return nil, fmt.Errorf("read DT_NEEDED: %w", err)
	}
	obj := &Object{Needed: needed, Requires: map[string][]string{}}
	if f.SectionByType(elf.SHT_GNU_VERSYM) == nil {
		return obj, nil
	}

	needs, err := f.DynamicVersionNeeds()
	if err != nil {
		return nil, fmt.Errorf("read version needs: %w", err)
	}
	for _, n := range needs {
		for _, d := range n.Needs {
			obj.Requires[n.Name] = append(obj.Requires[n.Name], d.Dep)
		}
	}
	defs, err := f.DynamicVersions()
	if err != nil {
		return nil, fmt.Errorf("read version definitions: %w", err)
	}
	for _, d := range defs {
		obj.Provides = append(obj.Provides, d.Name)
	}
	return obj, nil
}
