// Package rdma provides reliable byte streams over RDMA with the vocabulary
// of the net package.
//
// A connection is an rsocket, the stream socket of librdmacm (rsocket(7))
// carried over an RDMA queue pair, so it works over InfiniBand, RoCE and
// iWARP alike and talks to any rsockets application, and it is addressed
// like TCP: an IP address, which selects the device through rdma_cm, and a
// port. Dial connects to a listener:
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
// options, including a Config with the rsockets knobs of a connection: the
// sizes of its registered send and receive buffers, the depths of its work
// queues and the inline threshold. A zero field keeps the rsockets default,
// which /etc/rdma/rsocket configures for the process. There is no handshake
// of this package's own: the peer may be rstream, a program run under
// LD_PRELOAD=librspreload.so, or anything else built on rsockets.
//
// Every socket runs in non-blocking mode and one goroutine per process waits
// for all of them in rpoll, woken through a pipe when a connection starts
// waiting or closes; a Read or Write that cannot proceed parks its goroutine
// on a channel until the poller reports the socket ready, so idle connections
// neither spin nor hold threads. Each connection pins its two registered
// buffers, about 256 KiB with the defaults, which count against
// RLIMIT_MEMLOCK.
//
// Semantics follow the net package. The peer reads io.EOF once a side has
// called CloseWrite or Close and everything written before has been read.
// Close wakes blocked Reads and Writes with net.ErrClosed, deadlines expire
// with os.ErrDeadlineExceeded (Timeout reports true), and every error is a
// *net.OpError on net "rdma": errors.Is matches syscall.ECONNREFUSED when
// nobody listens, and errors.ErrUnsupported on platforms without librdmacm,
// where the package still compiles. Close and CloseWrite return at once
// while the peer is alive; when it has vanished, rsockets wait for the posted
// sends to be given up, which takes the transport's retry timeout, several
// seconds.
package rdma
