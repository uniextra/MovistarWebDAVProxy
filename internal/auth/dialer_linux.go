//go:build linux

package auth

import (
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// tcpUserTimeout: si lo enviado no se confirma en este tiempo, el kernel da la conexión
// por muerta (p. ej. se fue el cable y la conexión se quedó colgada). Sin esto Linux
// insiste ~15 minutos. Es el mismo plazo que el vigilante de subidas paradas.
const tcpUserTimeout = 60 * time.Second

// newDialer es el dialer de las conexiones con Movistar.
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 15 * time.Second,
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(tcpUserTimeout/time.Millisecond))
			})
			if err != nil {
				return err
			}
			return serr
		},
	}
}
