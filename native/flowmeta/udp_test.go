// Copyright 2026, Asterisk4Magisk contributors
// SPDX-License-Identifier: LGPL-3.0

package flowmeta

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

func testUDP(t testing.TB) (*UDPHub, *UDPConn) {
	t.Helper()
	accepted := make(chan *UDPConn, 1)
	release := make(chan struct{})
	hub := NewUDP(func(p []byte) (int, error) { return len(p), nil }, testToken, func(c *UDPConn) {
		accepted <- c
		<-release
	})
	t.Cleanup(func() {
		hub.Close()
		close(release)
	})
	packet, _ := testHeader(true).Marshal(testToken)
	if err := hub.Feed(buf.FromBytes(packet)); err != nil {
		t.Fatal(err)
	}
	flow := <-accepted
	p, _ := flow.ReadPacket()
	p.Release()
	return hub, flow
}

func TestUDPDatagramsAndReplies(t *testing.T) {
	hub, flow := testUDP(t)
	var reply []byte
	hub.write = func(p []byte) (int, error) {
		reply = append(reply[:0], p...)
		return len(p), nil
	}
	for _, size := range []int{0, 100, 60000} {
		prefix, _ := flow.Header.Marshal(testToken)
		payload := bytes.Repeat([]byte{42}, size)
		input := buf.FromBytes(append(prefix, payload...))
		if err := hub.Feed(input); err != nil {
			t.Fatal(err)
		}
		packet, err := flow.ReadPacket()
		if err != nil || packet != input || !bytes.Equal(packet.Bytes(), payload) {
			t.Fatal("wrong datagram", size, err)
		}
		packet.Release()
		remote := netip.MustParseAddrPort("203.0.113.4:443")
		if _, err := flow.WriteFrom(payload, remote); err != nil {
			t.Fatal(err)
		}
		h, n, err := Parse(reply, testToken, true)
		if err != nil || h.Source != remote || h.Destination != flow.Header.Source || !bytes.Equal(reply[n:], payload) {
			t.Fatal("wrong reply tuple", h, err)
		}
	}
}

func TestUDPFlowIsolation(t *testing.T) {
	accepted := make(chan Header, 3)
	release := make(chan struct{})
	hub := NewUDP(func(b []byte) (int, error) { return len(b), nil }, testToken, func(flow *UDPConn) {
		accepted <- flow.Header
		<-release
	})
	t.Cleanup(func() {
		hub.Close()
		close(release)
	})
	first := testHeader(true)
	otherOwner, otherDestination := first, first
	otherOwner.UID++
	otherDestination.Destination = netip.MustParseAddrPort("203.0.113.5:53")
	for _, header := range []Header{first, otherOwner, otherDestination} {
		packet, _ := header.Marshal(testToken)
		if err := hub.Feed(buf.FromBytes(packet)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-accepted:
			if got != header {
				t.Fatal("original flows merged", got, header)
			}
		case <-time.After(time.Second):
			t.Fatal("distinct flow was not accepted", header)
		}
	}
}

func TestUDPCloseUnblocksReader(t *testing.T) {
	_, flow := testUDP(t)
	result := make(chan error, 1)
	go func() {
		_, err := flow.ReadPacket()
		result <- err
	}()
	_ = flow.Close()
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock reader")
	}
}

func TestUDPFeedReleasesRejectedPackets(t *testing.T) {
	hub, flow := testUDP(t)
	header, _ := flow.Header.Marshal(testToken)
	header = append(header, 42)
	packet := func(data []byte) *buf.Buffer {
		p := buf.NewWithSize(int32(len(data)))
		_, _ = p.Write(data)
		return p
	}
	invalid := packet([]byte("invalid"))
	if hub.Feed(invalid) == nil || !invalid.IsEmpty() {
		t.Fatal("invalid datagram was not released")
	}
	for range cap(flow.packets) {
		if err := hub.Feed(packet(header)); err != nil {
			t.Fatal(err)
		}
	}
	full := packet(header)
	if err := hub.Feed(full); err != nil || !full.IsEmpty() {
		t.Fatal("full queue did not release the datagram", err)
	}
	hub.Close()
	closed := packet(header)
	if err := hub.Feed(closed); !errors.Is(err, net.ErrClosed) || !closed.IsEmpty() {
		t.Fatal("closed hub did not release the datagram", err)
	}
	if len(flow.packets) != 0 {
		t.Fatal("closed flow retained queued datagrams")
	}
}
