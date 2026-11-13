// Copyright 2026, Asterisk4Magisk contributors
// SPDX-License-Identifier: LGPL-3.0

package flowmeta

import (
	"bufio"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type packageUIDResolver func(string) int

var packageResolver atomic.Pointer[packageUIDResolver]
var rootPackages = packageCache{path: "/data/system/packages.list"}

// RegisterPackageUIDResolver is called before loading routing rules. JNI uses
// Android's PackageManager; only the standalone ROOT CLI reads packages.list.
func RegisterPackageUIDResolver(resolver func(string) int) {
	if resolver == nil {
		packageResolver.Store(nil)
		return
	}
	value := packageUIDResolver(resolver)
	packageResolver.Store(&value)
}

// PackageAppIDs compiles package aliases once per rule, leaving the JSON and
// the process finder's full UID unchanged. Shared-UID packages share an app ID.
func PackageAppIDs(names []string) []uint32 {
	resolver := packageResolver.Load()
	if resolver == nil {
		return nil
	}
	var appIDs []uint32
	for _, name := range names {
		if _, err := strconv.Atoi(name); err == nil || strings.Contains(name, "/") {
			continue
		}
		if uid := (*resolver)(name); uid >= 0 {
			appID := uint32(uid) % 100000
			if !slices.Contains(appIDs, appID) {
				appIDs = append(appIDs, appID)
			}
		}
	}
	return appIDs
}

func FindPackageUID(name string) int {
	return rootPackages.lookup(name)
}

type packageCache struct {
	path     string
	mu       sync.Mutex
	info     os.FileInfo
	packages map[string]int
}

func (c *packageCache) lookup(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := os.Stat(c.path)
	if err != nil {
		c.info, c.packages = nil, nil
		return -1
	}
	if c.info == nil || !os.SameFile(info, c.info) || info.Size() != c.info.Size() || !info.ModTime().Equal(c.info.ModTime()) {
		file, err := os.Open(c.path)
		if err != nil {
			return -1
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil {
			return -1
		}
		packages := make(map[string]int)
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			if uid, err := strconv.Atoi(fields[1]); err == nil && uid >= 0 {
				packages[fields[0]] = uid
			}
		}
		if scanner.Err() != nil {
			return -1
		}
		c.info, c.packages = info, packages
	}
	if uid, ok := c.packages[name]; ok {
		return uid
	}
	return -1
}
