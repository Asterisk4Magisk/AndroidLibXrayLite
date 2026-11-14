// Copyright 2026, Asterisk4Magisk contributors
// SPDX-License-Identifier: LGPL-3.0

package flowmeta

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPackageRulesAndSnapshotRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packages.list")
	cache := packageCache{path: path}
	if cache.lookup("com.example.app") != -1 {
		t.Fatal("missing package snapshot matched")
	}
	if err := os.WriteFile(path, []byte("com.example.app 12345\ncom.example.shared 12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	RegisterPackageUIDResolver(cache.lookup)
	t.Cleanup(func() { RegisterPackageUIDResolver(nil) })
	if ids := PackageAppIDs([]string{"com.example.app", "com.example.shared", "1012345", "xray/", "unknown"}); !slices.Equal(ids, []uint32{12345}) {
		t.Fatal(ids)
	}
	if err := os.WriteFile(path, []byte("com.example.app 12346\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if cache.lookup("com.example.app") != 12346 || cache.lookup("com.example.shared") != -1 {
		t.Fatal("package snapshot was not refreshed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if cache.lookup("com.example.app") != -1 {
		t.Fatal("removed package snapshot matched")
	}
}
