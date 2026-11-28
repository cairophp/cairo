package test

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

// The ports freeAddr hands out lie below the system's ephemeral range: a port
// from that range, freed for cairo to take, can be given meanwhile to an
// outgoing connection of any test (a client, a database), and cairo then
// fails to listen on it.
const lowestPort = 10000

var ports struct {
	sync.Mutex
	next, end int
}

// freeAddr is a local address that nothing listens on, and that no other
// call of this process got.
func freeAddr() string {
	ports.Lock()
	defer ports.Unlock()
	if ports.end == 0 {
		ports.end = ephemeralStart()
		// Test binaries that run at once start apart.
		ports.next = lowestPort + os.Getpid()%(ports.end-lowestPort)
	}
	for range ports.end - lowestPort {
		port := ports.next
		if ports.next++; ports.next >= ports.end {
			ports.next = lowestPort
		}
		addr := "127.0.0.1:" + strconv.Itoa(port)
		if ln, err := net.Listen("tcp", addr); err == nil {
			ln.Close()
			return addr
		}
	}
	panic(fmt.Sprintf("no free port between %d and %d", lowestPort, ports.end))
}

// ephemeralStart is where the system's ports for outgoing connections begin.
func ephemeralStart() int {
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		if f := strings.Fields(string(b)); len(f) == 2 {
			if n, err := strconv.Atoi(f[0]); err == nil && n > lowestPort+1000 {
				return n
			}
		}
	}
	return 32768
}
