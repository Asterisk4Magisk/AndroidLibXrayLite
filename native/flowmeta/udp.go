// Copyright 2026, Asterisk4Magisk contributors
// SPDX-License-Identifier: LGPL-3.0

package flowmeta

import (
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

// UDPHub separates the original flows sharing a local bridge socket.
type UDPHub struct {
	mu     sync.Mutex
	flows  map[Header]*UDPConn
	closed bool
	write  func([]byte) (int, error)
	token  string
	accept func(*UDPConn)
}

func NewUDP(write func([]byte) (int, error), token string, accept func(*UDPConn)) *UDPHub {
	return &UDPHub{
		flows:  make(map[Header]*UDPConn),
		write:  write,
		token:  token,
		accept: accept,
	}
}

// Feed takes ownership of packet, including when it rejects the datagram.
func (s *UDPHub) Feed(packet *buf.Buffer) error {
	h, n, err := Parse(packet.Bytes(), s.token, true)
	if err != nil {
		packet.Release()
		return err
	}
	packet.Advance(int32(n))
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		packet.Release()
		return net.ErrClosed
	}
	c := s.flows[h]
	created := c == nil
	if c == nil {
		c = &UDPConn{
			Header:  h,
			hub:     s,
			packets: make(chan *buf.Buffer, 64),
			done:    make(chan struct{}),
		}
		s.flows[h] = c
	}
	// Keep enqueue and close serialized; payload storage belongs to the reader.
	select {
	case c.packets <- packet:
		packet = nil
	default:
	}
	s.mu.Unlock()
	packet.Release()
	if created {
		go func() {
			defer c.Close()
			s.accept(c)
		}()
	}
	return nil
}

func (s *UDPHub) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, c := range s.flows {
		c.closeLocked()
	}
	clear(s.flows)
}

type UDPConn struct {
	Header    Header
	hub       *UDPHub
	packets   chan *buf.Buffer
	done      chan struct{}
	once      sync.Once
	readTimer *time.Timer // Only the packet reader uses this timer.
}

// ReadPacket transfers ownership of a complete datagram to its caller.
func (c *UDPConn) ReadPacket() (*buf.Buffer, error) {
	select {
	case <-c.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case packet := <-c.packets:
		return packet, nil
	default:
	}
	if c.readTimer == nil {
		c.readTimer = time.NewTimer(time.Minute)
	} else {
		c.readTimer.Reset(time.Minute)
	}
	defer c.readTimer.Stop()
	select {
	case <-c.done:
		return nil, net.ErrClosed
	case <-c.readTimer.C:
		return nil, os.ErrDeadlineExceeded
	case packet := <-c.packets:
		return packet, nil
	}
}

func (c *UDPConn) Write(b []byte) (int, error) { return c.WriteFrom(b, c.Header.Destination) }

func (c *UDPConn) WriteFrom(b []byte, source netip.AddrPort) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	packet := buf.NewWithSize(int32(MaxHeader + len(b)))
	defer packet.Release()
	n, err := c.Header.Reverse(source).MarshalTo(packet.Extend(MaxHeader), c.hub.token)
	if err != nil {
		return 0, err
	}
	packet.Resize(0, int32(n))
	_, _ = packet.Write(b)
	if _, err = c.hub.write(packet.Bytes()); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *UDPConn) closeLocked() {
	c.once.Do(func() { close(c.done) })
	for {
		select {
		case packet := <-c.packets:
			packet.Release()
		default:
			return
		}
	}
}

func (c *UDPConn) Close() error {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	c.closeLocked()
	if c.hub.flows[c.Header] == c {
		delete(c.hub.flows, c.Header)
	}
	return nil
}
