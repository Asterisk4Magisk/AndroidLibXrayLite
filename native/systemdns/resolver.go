package systemdns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
)

// Android has no resolv.conf, so Go's resolver dials its built-in loopback
// defaults. Dial ignores that address and reaches the observed servers from
// the core's own sockets, which ROOT routing exempts by owner.
var servers atomic.Pointer[[]netip.AddrPort]

var errNoServers = errors.New("no system DNS servers")

// decodeSnapshot already normalized and validated every endpoint.
func setServers(snapshot []string) {
	endpoints := make([]netip.AddrPort, 0, len(snapshot))
	for _, server := range snapshot {
		endpoints = append(endpoints, netip.MustParseAddrPort(server))
	}
	servers.Store(&endpoints)
}

func dial(ctx context.Context, network, _ string) (net.Conn, error) {
	current := servers.Load()
	if current == nil || len(*current) == 0 {
		return nil, errNoServers
	}
	if network == "udp" || network == "udp4" || network == "udp6" {
		var config net.ListenConfig
		connection, err := config.ListenPacket(ctx, "udp", ":0")
		if err != nil {
			return nil, err
		}
		return &fanOutConn{UDPConn: connection.(*net.UDPConn), servers: *current}, nil
	}
	var (
		dialer net.Dialer
		err    error
	)
	for _, server := range *current {
		var connection net.Conn
		if connection, err = dialer.DialContext(ctx, network, server.String()); err == nil {
			return connection, nil
		}
	}
	return nil, err
}

// fanOutConn sends each query to every server and returns the first reply,
// like resolvers that do not wait for a dead first server to time out. It is
// a PacketConn, so Go keeps datagram framing and validates the reply itself.
type fanOutConn struct {
	*net.UDPConn
	servers []netip.AddrPort
}

func (c *fanOutConn) Write(query []byte) (int, error) {
	var sent bool
	var err error
	for _, server := range c.servers {
		if _, writeErr := c.WriteToUDPAddrPort(query, server); writeErr == nil {
			sent = true
		} else {
			err = writeErr
		}
	}
	if !sent {
		return 0, err
	}
	return len(query), nil
}

func (c *fanOutConn) Read(reply []byte) (int, error) {
	for {
		n, from, err := c.ReadFromUDPAddrPort(reply)
		if err != nil {
			return n, err
		}
		for _, server := range c.servers {
			if from.Port() == server.Port() &&
				from.Addr().Unmap().WithZone("") == server.Addr().Unmap().WithZone("") {
				return n, nil
			}
		}
	}
}

func (c *fanOutConn) RemoteAddr() net.Addr {
	return net.UDPAddrFromAddrPort(c.servers[0])
}
