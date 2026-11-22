//go:build !linux

package auth

import (
	"net"
	"time"
)

// newDialer es el dialer de las conexiones con Movistar (TCP_USER_TIMEOUT solo en Linux).
func newDialer() *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 15 * time.Second}
}
