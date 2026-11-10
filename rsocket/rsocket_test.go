package rsocket_test

import (
	"fmt"
	"testing"

	"github.com/wzshiming/rdma/rsocket"
)

func TestStringers(t *testing.T) {
	tests := []struct {
		v    fmt.Stringer
		want string
	}{
		{rsocket.IPv4, "AF_INET"},
		{rsocket.IPv6, "AF_INET6"},
		{rsocket.Domain(7), "Domain(7)"},
		{rsocket.ShutRead, "SHUT_RD"},
		{rsocket.ShutWrite, "SHUT_WR"},
		{rsocket.ShutReadWrite, "SHUT_RDWR"},
		{rsocket.How(3), "How(3)"},
		{rsocket.PollIn, "POLLIN"},
		{rsocket.PollOut, "POLLOUT"},
		{rsocket.PollErr, "POLLERR"},
		{rsocket.PollHup, "POLLHUP"},
		{rsocket.PollNval, "POLLNVAL"},
		{rsocket.PollIn | rsocket.PollOut, "POLLIN|POLLOUT"},
		{rsocket.PollErr | rsocket.Events(0x100), "POLLERR|0x100"},
		{rsocket.Events(0), "0"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%T(%d).String() = %q, want %q", tt.v, tt.v, got, tt.want)
		}
	}
}
