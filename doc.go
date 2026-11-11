// Package rdma provides reliable byte streams over RDMA with the vocabulary
// of the net package.
//
// A connection speaks the stream protocol of rsockets (rsocket(7)) over an
// RDMA queue pair, so it talks to any rsockets application, and it is
// addressed like TCP: an IP address, which selects the device through
// rdma_cm, and a port. Dial connects to a listener:
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
// options, including a Config with the engine and the rsockets knobs of a
// connection: the sizes of its registered send and receive buffers, the
// depths of its work queues and the inline threshold. There is no handshake
// of this package's own: the peer may be rstream, a program run under
// LD_PRELOAD=librspreload.so, or anything else built on rsockets, and it
// cannot tell which engine it talks to.
//
// Config.Engine picks the engine that carries the connections; a Listener
// accepts connections of its own engine, whatever the dialer runs. The
// native engine, the default, runs the rsockets protocol in Go on the verbs
// and cm packages, over InfiniBand and RoCE but not iWARP, where a dial fails
// with an error matching errors.ErrUnsupported and a listener turns the
// requests away. Each of its connections has a few goroutines of its own,
// parked on the runtime poller while idle, which take in what arrives
// without waiting for a call, so that an idle connection learns of the
// peer's close; connections share no poller. Its zero knobs are the
// compile-time defaults of rsockets: /etc/rdma/rsocket is not read.
// EngineLibrdmacm selects the librdmacm engine, which drives librdmacm's own
// rsockets, iWARP included: every socket runs in non-blocking mode, one
// goroutine per process waits for all of them in rpoll, woken through a pipe
// when a connection starts waiting or closes, zero knobs keep the rsockets
// defaults, which /etc/rdma/rsocket configures for the process, and before
// rdma-core 53 no more than three connections in one process are reliable.
// On either engine a Read or Write that cannot proceed parks its goroutine
// until the connection is ready, so idle connections neither spin nor hold
// threads, and each connection pins its two registered buffers, about 256
// KiB with the defaults, which count against RLIMIT_MEMLOCK.
//
// Semantics follow the net package. The peer reads io.EOF once a side has
// called CloseWrite or Close and everything written before has been read.
// Close wakes blocked Reads and Writes with net.ErrClosed, deadlines expire
// with os.ErrDeadlineExceeded (Timeout reports true), and every error is a
// *net.OpError on net "rdma": errors.Is matches syscall.ECONNREFUSED when
// nobody listens, and errors.ErrUnsupported on platforms without rdma-core,
// where the package still compiles, or when libibverbs.so.1 or
// librdmacm.so.1 cannot be loaded at run time. A connection lost without the
// peer's close reads as syscall.ECONNRESET on the native engine, once what
// arrived before is read, and as io.EOF on the librdmacm engine, as rsockets
// report it. Close and CloseWrite return at once while the peer is alive;
// when it has vanished, the native engine's Close gives the posted sends at
// most a second, and the librdmacm engine waits for them to be given up,
// which takes the transport's retry timeout, several seconds.
//
// *Conn implements io.ReaderFrom and io.WriterTo, which io.Copy uses in
// either direction. ReadFrom sends what the source reads, each read as soon
// as it returns, and returns nil at the source's io.EOF. WriteTo hands the
// destination what has arrived, all that lies contiguous at the time, and
// returns nil once the peer has shut down writing and everything is handed
// over, or io.ErrShortWrite when the destination takes less without an
// error. Their errors are *net.OpError values of Op "readfrom" and
// "writeto", which carry the source's and the destination's own. ReadFrom
// holds the sending side and WriteTo the receiving side until they return;
// Close and CloseWrite do not wait for them, and the deadlines bound their
// waits for room and data but not the source and the destination. Whether
// the bytes move without a copy depends on the engine. The native engine
// lends its registered buffers: the source reads straight into the send
// buffer and the destination is handed the receive buffer, whose room
// returns to the peer only once the destination has taken the bytes; what a
// failing destination did not take stays to be read. librdmacm's rsockets
// keep their buffers to themselves, so over them both methods move the bytes
// through a buffer of their own, which returns the receive room before the
// destination takes the bytes and loses those it refuses.
package rdma
