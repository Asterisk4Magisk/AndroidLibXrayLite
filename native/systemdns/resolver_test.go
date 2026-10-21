//go:build android || linux

package systemdns

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// answer replies to every A query with 192.0.2.7 and to anything else with no records.
func answer(t *testing.T, connection *net.UDPConn) {
	buffer := make([]byte, 512)
	for {
		n, from, err := connection.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return
		}
		end := 12
		for end < n && buffer[end] != 0 {
			end += int(buffer[end]) + 1
		}
		end += 5 // root label, QTYPE, QCLASS; drops the query's EDNS record
		if end > n {
			continue
		}
		reply := append([]byte(nil), buffer[:end]...)
		reply[2] |= 0x80 // QR
		reply[3] = 0x80  // RA, NOERROR
		reply[10], reply[11] = 0, 0
		if reply[end-4] == 0 && reply[end-3] == 1 {
			reply[7] = 1
			reply = append(reply, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 7)
		}
		if _, err := connection.WriteToUDPAddrPort(reply, from); err != nil {
			t.Error(err)
		}
	}
}

func listen(t *testing.T) *net.UDPConn {
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func TestFanOutSkipsSilentServer(t *testing.T) {
	silent, live := listen(t), listen(t)
	go answer(t, live)
	servers.Store(&[]netip.AddrPort{
		silent.LocalAddr().(*net.UDPAddr).AddrPort(), live.LocalAddr().(*net.UDPAddr).AddrPort()})
	defer servers.Store(nil)
	resolver := &net.Resolver{PreferGo: true, Dial: dial}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addresses, err := resolver.LookupNetIP(ctx, "ip4", "fanout.invalid.")
	if err != nil || len(addresses) != 1 || addresses[0] != netip.MustParseAddr("192.0.2.7") {
		t.Fatalf("lookup through the live server: %v %v", addresses, err)
	}
}

func TestNoServersFailsLookup(t *testing.T) {
	setServers(nil)
	defer servers.Store(nil)
	resolver := &net.Resolver{PreferGo: true, Dial: dial}
	if _, err := resolver.LookupNetIP(context.Background(), "ip4", "empty.invalid."); err == nil {
		t.Fatal("lookup succeeded without servers")
	}
}
