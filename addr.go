package rdma

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
)

// Addr is the address of an RDMA stream endpoint: the IP address that selects
// the device and the rdma_cm port, like net.TCPAddr.
type Addr struct {
	IP   net.IP
	Port int
	Zone string // IPv6 scoped addressing zone
}

// Network returns the address's network name, "rdma".
func (a *Addr) Network() string { return "rdma" }

// String returns the address in host:port form.
func (a *Addr) String() string {
	if a == nil {
		return "<nil>"
	}
	ip := ""
	if len(a.IP) != 0 {
		ip = a.IP.String()
		if a.Zone != "" {
			ip += "%" + a.Zone
		}
	}
	return net.JoinHostPort(ip, strconv.Itoa(a.Port))
}

// addrPort converts the address for the rsocket package; a nil IP gives the
// zero AddrPort.
func (a *Addr) addrPort() netip.AddrPort {
	if a == nil {
		return netip.AddrPort{}
	}
	ip, ok := netip.AddrFromSlice(a.IP)
	if !ok {
		return netip.AddrPort{}
	}
	ip = ip.Unmap()
	if a.Zone != "" {
		ip = ip.WithZone(a.Zone)
	}
	return netip.AddrPortFrom(ip, uint16(a.Port))
}

// addrFromAddrPort converts an address reported by the rsocket package; the
// zero AddrPort gives nil.
func addrFromAddrPort(ap netip.AddrPort) *Addr {
	if !ap.IsValid() {
		return nil
	}
	ip := ap.Addr().Unmap()
	return &Addr{IP: ip.WithZone("").AsSlice(), Port: int(ap.Port()), Zone: ip.Zone()}
}

// ResolveAddr returns the address of an RDMA stream endpoint. The network
// must be "rdma", "rdma4" or "rdma6" and the address of the form host:port;
// a host name is resolved with net.DefaultResolver and the first address of
// the network's family is used, and an empty host means the unspecified
// address. Errors are *net.OpError values with Op "resolve".
func ResolveAddr(network, address string) (*Addr, error) {
	return resolveAddr(context.Background(), network, address)
}

// resolveAddr implements ResolveAddr with a context for the resolver.
func resolveAddr(ctx context.Context, network, address string) (*Addr, error) {
	addr, err := resolve(ctx, network, address)
	if err != nil {
		return nil, &net.OpError{Op: "resolve", Net: "rdma", Err: err}
	}
	return addr, nil
}

// family reports the address family a network name allows: 0 for both.
func family(network string) (int, error) {
	switch network {
	case "rdma":
		return 0, nil
	case "rdma4":
		return 4, nil
	case "rdma6":
		return 6, nil
	}
	return 0, net.UnknownNetworkError(network)
}

// resolve turns host:port into an Addr, looking the host up when needed.
func resolve(ctx context.Context, network, address string) (*Addr, error) {
	fam, err := family(network)
	if err != nil {
		return nil, err
	}
	if address == "" {
		return nil, errors.New("missing address")
	}
	host, service, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := net.DefaultResolver.LookupPort(ctx, "tcp", service)
	if err != nil {
		return nil, err
	}
	addr := &Addr{Port: port}
	if host == "" {
		return addr, nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !familyOK(ip, fam) {
			return nil, &net.AddrError{Err: "no suitable address found", Addr: host}
		}
		addr.IP, addr.Zone = ip.Unmap().WithZone("").AsSlice(), ip.Zone()
		return addr, nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if familyOK(ip, fam) {
			addr.IP, addr.Zone = ip.Unmap().WithZone("").AsSlice(), ip.Zone()
			return addr, nil
		}
	}
	return nil, &net.AddrError{Err: "no suitable address found", Addr: host}
}

func familyOK(ip netip.Addr, fam int) bool {
	switch fam {
	case 4:
		return ip.Unmap().Is4()
	case 6:
		return ip.Is6() && !ip.Is4In6()
	}
	return true
}
