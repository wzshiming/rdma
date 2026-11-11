package rdma

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rsproto"
	"github.com/wzshiming/rdma/verbs"
)

// A native connection is sized as rsockets sizes an rsocket that the
// librdmacm engine set the same Config on: buffer sizes doubled, a send
// buffer of 4096 bytes when that falls short of RS_SNDLOWAT, queue depths and
// the inline size at most 0xFFFE, queue depths at least 16 and within the
// device's limit, and the compile-time defaults of rsockets for zero.
func TestNativeSizesFollowRsockets(t *testing.T) {
	const roomy = 1 << 20 // a device limit beyond every depth asked for
	for _, tt := range []struct {
		name     string
		c        Config
		maxQueue int
		want     connSizes
	}{
		{"defaults", Config{}, roomy, connSizes{1 << 17, 1 << 17, 384, 384, 64}},
		{"buffers doubled", Config{SendBufferSize: 1500, ReceiveBufferSize: 3}, roomy, connSizes{3000, 6, 384, 384, 64}},
		{"send buffer short of RS_SNDLOWAT", Config{SendBufferSize: 1023}, roomy, connSizes{4096, 1 << 17, 384, 384, 64}},
		{"send buffer at RS_SNDLOWAT", Config{SendBufferSize: 1024}, roomy, connSizes{2048, 1 << 17, 384, 384, 64}},
		{"queues below 16", Config{SendQueueSize: 1, ReceiveQueueSize: 15}, roomy, connSizes{1 << 17, 1 << 17, 16, 16, 64}},
		{"queues beyond the device", Config{SendQueueSize: 2000, ReceiveQueueSize: 1001}, 1000, connSizes{1 << 17, 1 << 17, 1000, 1000, 64}},
		{"defaults beyond the device", Config{}, 100, connSizes{1 << 17, 1 << 17, 100, 100, 64}},
		{"queues beyond rsockets", Config{SendQueueSize: 1 << 20, ReceiveQueueSize: 0xFFFF}, roomy, connSizes{1 << 17, 1 << 17, 0xFFFE, 0xFFFE, 64}},
		{"inline", Config{MaxInline: 1}, roomy, connSizes{1 << 17, 1 << 17, 384, 384, 1}},
		{"inline beyond rsockets", Config{MaxInline: 1 << 20}, roomy, connSizes{1 << 17, 1 << 17, 384, 384, 0xFFFE}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sizesFor(tt.c, tt.maxQueue)
			if err != nil || got != tt.want {
				t.Errorf("sizesFor(%+v, %d) = %+v, %v; want %+v, nil", tt.c, tt.maxQueue, got, err, tt.want)
			}
		})
	}
}

// A buffer whose doubled size the 32 bits of rsockets cannot hold is refused
// as an invalid Config.
func TestNativeSizesRefuseBuffersBeyond32Bits(t *testing.T) {
	if math.MaxInt == math.MaxInt32 {
		t.Skip("an int holds no such size here")
	}
	fits := math.MaxUint32 / 2
	for _, c := range []Config{{SendBufferSize: fits + 1}, {ReceiveBufferSize: fits + 1}} {
		if got, err := sizesFor(c, 1<<20); !errors.Is(err, errConfig) {
			t.Errorf("sizesFor(%+v) = %+v, %v; want an error matching errConfig", c, got, err)
		}
	}
	c := Config{SendBufferSize: fits, ReceiveBufferSize: fits}
	if got, err := sizesFor(c, 1<<20); err != nil || got.sendBuffer != 2*fits || got.receiveBuffer != 2*fits {
		t.Errorf("sizesFor(%+v) = %+v, %v; want buffers of %d bytes", c, got, err, 2*fits)
	}
}

// A native listen refuses a Config whose buffers the 32 bits of rsockets
// cannot hold before it listens, rather than refuse every request.
func TestNativeListenRefusesBuffersBeyond32Bits(t *testing.T) {
	if math.MaxInt == math.MaxInt32 {
		t.Skip("an int holds no such size here")
	}
	fits := math.MaxUint32 / 2
	lc := ListenConfig{Config: Config{ReceiveBufferSize: fits + 1}, eng: nativeEngine{}}
	ln, err := lc.Listen(t.Context(), "rdma", "192.0.2.1:7471")
	if err == nil {
		ln.Close()
	}
	if !isOpError(err, "listen", errConfig) {
		t.Errorf("Listen() with %+v error = %v, want a listen *net.OpError matching errConfig", lc.Config, err)
	}
}

// The native engine refuses a device of the iWARP transport, whose mode of
// rsockets it leaves out, with an error matching errors.ErrUnsupported that
// points at the librdmacm engine, and takes every other one, as rs_create_ep
// sets up only iWARP differently.
func TestNativeEngineRefusesIWARPDevices(t *testing.T) {
	for _, tt := range []struct {
		transport verbs.TransportType
		refused   bool
	}{
		{verbs.TransportIB, false},
		{verbs.TransportIWARP, true},
		{verbs.TransportUSNIC, false},
		{verbs.TransportUSNICUDP, false},
		{verbs.TransportUnspecified, false},
		{verbs.TransportUnknown, false},
	} {
		t.Run(tt.transport.String(), func(t *testing.T) {
			e := nativeEngine{transportType: func(*verbs.Context) verbs.TransportType { return tt.transport }}
			err := e.checkDevice(nil)
			switch {
			case !tt.refused && err != nil:
				t.Errorf("checkDevice on %v = %v, want nil", tt.transport, err)
			case tt.refused && (!errors.Is(err, errors.ErrUnsupported) || !strings.Contains(fmt.Sprint(err), "librdmacm engine")):
				t.Errorf("checkDevice on %v = %v, want an error matching errors.ErrUnsupported that names the librdmacm engine", tt.transport, err)
			}
		})
	}
}

// A native listener admits a connection request as rs_accept does, only
// with connection data of version 1, and on no iWARP device, refusing it
// with an error that points at the librdmacm engine: the rest it rejects.
func TestNativeListenerRejectsRequests(t *testing.T) {
	// rdma_cm hands out the private data of a request zero padded.
	request := func(version byte) []byte {
		data := rsproto.ConnData{Credits: 384}.Encode()
		data[0] = version
		return append(data[:], make([]byte, cm.MaxConnectPrivateData-len(data))...)
	}
	for _, tt := range []struct {
		name      string
		data      []byte
		transport verbs.TransportType
		want      error // nil when admitted
	}{
		{"version 1 on InfiniBand", request(1), verbs.TransportIB, nil},
		{"version 1 on usNIC", request(1), verbs.TransportUSNIC, nil},
		{"version 0", request(0), verbs.TransportIB, rsproto.ErrVersion},
		{"version 2", request(2), verbs.TransportIB, rsproto.ErrVersion},
		{"version 255", request(255), verbs.TransportIB, rsproto.ErrVersion},
		{"no connection data", make([]byte, cm.MaxConnectPrivateData), verbs.TransportIB, rsproto.ErrVersion},
		{"version 1 on iWARP", request(1), verbs.TransportIWARP, errIWARP},
		{"version 2 on iWARP", request(2), verbs.TransportIWARP, rsproto.ErrVersion},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := nativeEngine{transportType: func(*verbs.Context) verbs.TransportType { return tt.transport }}
			peer, err := e.admit(tt.data, nil)
			switch {
			case tt.want == nil && (err != nil || peer.Credits != 384):
				t.Errorf("admit = %+v, %v; want the request's connection data, nil", peer, err)
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Errorf("admit error = %v, want %v", err, tt.want)
			}
		})
	}
}

// A native dial fails at once with a dial *net.OpError matching
// errors.ErrUnsupported when one of the libraries it runs on, the first or
// the second it loads, cannot be loaded.
func TestNativeDialWithoutALibraryIsUnsupported(t *testing.T) {
	const missing = "/nonexistent/"
	for _, tt := range []struct {
		name      string
		libraries []string
	}{
		{"libibverbs", []string{missing + "libibverbs.so.1", "librdmacm.so.1"}},
		{"librdmacm", []string{"libibverbs.so.1", missing + "librdmacm.so.1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := Dialer{eng: nativeEngine{libraries: tt.libraries}}
			c, err := d.Dial("rdma", "192.0.2.1:7471")
			if err == nil {
				c.Close()
			}
			if !isOpError(err, "dial", errors.ErrUnsupported) {
				t.Errorf("Dial() loading %q error = %v, want a dial *net.OpError matching errors.ErrUnsupported", tt.libraries, err)
			}
			t.Logf("Dial() loading %q: %v", tt.libraries, err)
		})
	}
}

// A native listen fails at once with a listen *net.OpError matching
// errors.ErrUnsupported when one of the libraries it runs on, the first or
// the second it loads, cannot be loaded.
func TestNativeListenWithoutALibraryIsUnsupported(t *testing.T) {
	const missing = "/nonexistent/"
	for _, tt := range []struct {
		name      string
		libraries []string
	}{
		{"libibverbs", []string{missing + "libibverbs.so.1", "librdmacm.so.1"}},
		{"librdmacm", []string{"libibverbs.so.1", missing + "librdmacm.so.1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lc := ListenConfig{eng: nativeEngine{libraries: tt.libraries}}
			ln, err := lc.Listen(t.Context(), "rdma", "192.0.2.1:7471")
			if err == nil {
				ln.Close()
			}
			if !isOpError(err, "listen", errors.ErrUnsupported) {
				t.Errorf("Listen() loading %q error = %v, want a listen *net.OpError matching errors.ErrUnsupported", tt.libraries, err)
			}
			t.Logf("Listen() loading %q: %v", tt.libraries, err)
		})
	}
}
