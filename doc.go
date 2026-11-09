// Package rdma provides reliable byte streams over RDMA with the vocabulary
// of the net package.
//
// A connection is an RC queue pair driven with SEND and RECV only, so it
// works over InfiniBand, RoCE and iWARP alike, and it is addressed like TCP:
// an IP address, which selects the device through rdma_cm, and a port. Dial
// connects to a listener:
//
//	conn, err := rdma.Dial("rdma", "10.200.0.1:4791")
//	if err != nil {
//		// handle error
//	}
//	fmt.Fprintf(conn, "GET / HTTP/1.0\r\n\r\n")
//	status, err := bufio.NewReader(conn).ReadString('\n')
//	// ...
//
// Listen announces on an address, port 0 picking a free one, and Accept
// hands out connections:
//
//	ln, err := rdma.Listen("rdma", "10.200.0.1:4791")
//	if err != nil {
//		// handle error
//	}
//	for {
//		conn, err := ln.Accept()
//		if err != nil {
//			// handle error
//		}
//		go handleConnection(conn)
//	}
//
// The networks are "rdma", "rdma4" and "rdma6", the latter two restricting
// the address family; host names are resolved with net.DefaultResolver.
// *Conn implements net.Conn and adds CloseWrite, *Listener implements
// net.Listener and adds AcceptRDMA, and Dialer and ListenConfig carry the
// options, including a Config that sizes the buffers of a connection: its
// message size and receive depth are negotiated down to the smaller of the
// two peers' values during the handshake, which rides in the connection
// manager's private data.
//
// Data is copied into registered send buffers by Write and out of registered
// receive buffers by Read; the number of messages in flight is bounded by
// credits the receiver grants as its buffers are read, so no message is ever
// sent without a buffer waiting for it. Each connection pins
// (SendDepth+ReceiveDepth+2) x MessageSize bytes, about 1 MiB with the
// defaults, which count against RLIMIT_MEMLOCK. Every wait, for completions
// and for connection events alike, goes through the Go runtime poller: idle
// connections neither spin nor occupy threads.
//
// Semantics follow the net package. The peer reads io.EOF once a side has
// called CloseWrite or Close and everything written before has been read.
// Close wakes blocked Reads and Writes with net.ErrClosed, deadlines expire
// with os.ErrDeadlineExceeded (Timeout reports true), and every error is a
// *net.OpError on net "rdma": errors.Is matches syscall.ECONNREFUSED when
// nobody listens, syscall.ECONNRESET when the peer vanished without sending
// EOF, syscall.ETIMEDOUT when the transport's retries ran out, ErrProtocol
// when the peer speaks another protocol version, and errors.ErrUnsupported on
// platforms without librdmacm, where the package still compiles.
package rdma
