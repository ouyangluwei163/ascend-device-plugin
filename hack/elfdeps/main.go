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

// Command elfdeps fails when shared objects inside a (foreign-architecture)
// rootfs would not load: a DT_NEEDED missing from the search path, or a
// required symbol version (e.g. GLIBC_2.34) its provider does not define.
// The Dockerfile runs it at build time in place of the target's dynamic linker,
// which cannot execute on the build platform.
//
//	elfdeps -root /sysroot -path /usr/lib64:/usr/lib/aarch64-linux-gnu liburma.so.0 /usr/lib64/urma/liburma-udma.so
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Project-HAMi/ascend-device-plugin/internal/elfdeps"
)

func main() {
	root := flag.String("root", "/", "host directory holding the target rootfs")
	searchPath := flag.String("path", "", "colon-separated library directories inside root, in lookup order")
	flag.Parse()
	if flag.NArg() == 0 || *searchPath == "" {
		fmt.Fprintln(os.Stderr, "usage: elfdeps -root DIR -path DIR[:DIR...] LIB|PATH...")
		os.Exit(2)
	}

	c := elfdeps.Checker{Root: *root, SearchPath: strings.Split(*searchPath, ":")}
	errs := c.Check(flag.Args()...)
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, "elfdeps:", err)
	}
	if len(errs) > 0 {
		os.Exit(1)
	}
	fmt.Printf("elfdeps: %s resolve under %s\n", strings.Join(flag.Args(), " "), *root)
}
