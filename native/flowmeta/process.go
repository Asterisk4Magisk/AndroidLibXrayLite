// Copyright 2026, Asterisk4Magisk contributors
// SPDX-License-Identifier: LGPL-3.0

package flowmeta

import (
	"strconv"
	"sync"
)

type ProcessFinder func(network, source string, sourcePort uint16, destination string, destinationPort uint16) (int, string, string, error)

// ProcessLookup retains the capture tuple and memoizes attribution for one flow.
// Failed lookups are also final for this flow, avoiding repeated binder/procfs
// queries when several process rules inspect the same connection.
func ProcessLookup(h Header, finder ProcessFinder) func() (int, string, string, error) {
	return MemoizeProcessLookup(func() (int, string, string, error) {
		if h.UID != UnknownUID {
			return int(h.UID), strconv.FormatUint(uint64(h.UID), 10), "", nil
		}
		network := "tcp"
		if h.UDP {
			network = "udp"
		}
		return finder(network, h.Source.Addr().String(), h.Source.Port(), h.Destination.Addr().String(), h.Destination.Port())
	})
}

// MemoizeProcessLookup shares one lookup across the rules evaluating a flow.
func MemoizeProcessLookup(lookup func() (int, string, string, error)) func() (int, string, string, error) {
	var cached struct {
		once       sync.Once
		uid        int
		name, path string
		err        error
	}
	return func() (int, string, string, error) {
		cached.once.Do(func() {
			cached.uid, cached.name, cached.path, cached.err = lookup()
		})
		return cached.uid, cached.name, cached.path, cached.err
	}
}
