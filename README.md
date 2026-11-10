# rdma

[![Go Reference](https://pkg.go.dev/badge/github.com/wzshiming/rdma.svg)](https://pkg.go.dev/github.com/wzshiming/rdma)
[![Go](https://github.com/wzshiming/rdma/actions/workflows/go.yml/badge.svg)](https://github.com/wzshiming/rdma/actions/workflows/go.yml)

Reliable byte streams over RDMA with the vocabulary of the `net` package: `Dial` and `Listen` give out `net.Conn` and `net.Listener` values backed by rsockets, the stream sockets of librdmacm, addressed by IP address and port like TCP, working over InfiniBand, RoCE and iWARP alike, and depending on nothing but the standard library and rdma-core.

## Requirements

- Linux with the kernel's RDMA stack (`ib_uverbs`, `rdma_ucm`) and either an RDMA device or Soft-RoCE (the `rdma_rxe` kernel module) over any Ethernet interface; see [Soft-RoCE](#soft-roce) for a one-host setup.
- rdma-core: the headers `libibverbs-dev` and `librdmacm-dev` to build, the libraries `libibverbs1`, `librdmacm1t64` and `ibverbs-providers` to run (Debian and Ubuntu package names; the library is `librdmacm1` before Debian trixie and Ubuntu 24.04). Release 53 or later holds more than three connections in one process reliably: earlier releases, the 50.0 of Ubuntu 24.04 among them, have the rsockets `cm_svc_run` bug, a service thread polling a freed descriptor array once a fourth rsocket registers, which hangs connections or crashes the process.
- A locked-memory limit that holds the registered buffers, which are pinned: about 256 KiB per connection with the rsockets defaults, so the usual 8 MiB limit holds a few dozen connections. `ulimit -l unlimited` raises it for a shell.

Everywhere else the module compiles to stubs; see [Platforms](#platforms).

## Install

```sh
go get github.com/wzshiming/rdma
go install github.com/wzshiming/rdma/cmd/rdma@latest
```

## Usage

Dial connects to a server like `net.Dial` would, and the connection is a `net.Conn`:

```go
conn, err := rdma.Dial("rdma", "10.200.0.1:4791")
if err != nil {
	log.Fatal(err)
}
defer conn.Close()

fmt.Fprintf(conn, "GET / HTTP/1.0\r\n\r\n")
status, err := bufio.NewReader(conn).ReadString('\n')
if err != nil {
	log.Fatal(err)
}
fmt.Print(status)
```

Listen announces an address, port 0 picking a free one, and Accept hands out connections:

```go
ln, err := rdma.Listen("rdma", "10.200.0.1:4791")
if err != nil {
	log.Fatal(err)
}
defer ln.Close()

for {
	conn, err := ln.Accept()
	if err != nil {
		log.Fatal(err)
	}
	go func(c net.Conn) {
		defer c.Close()
		io.Copy(c, c)
	}(conn)
}
```

The networks are `rdma`, `rdma4` and `rdma6`; the IP address selects the device through rdma_cm. `Dialer` and `ListenConfig` carry the options, including a `Config` with the rsockets knobs of a connection, and every error is a `*net.OpError` on net `rdma`, so `errors.Is` matches `syscall.ECONNREFUSED`, `os.ErrDeadlineExceeded` and `net.ErrClosed` as it does for TCP. The stream is the rsockets protocol itself, so the peer may be any rsockets application, `rstream` or a program run under `LD_PRELOAD=librspreload.so` included, and the defaults of a process come from `/etc/rdma/rsocket`. The [package documentation](https://pkg.go.dev/github.com/wzshiming/rdma) has the rest.

## Packages

- [`rsocket`](https://pkg.go.dev/github.com/wzshiming/rdma/rsocket) binds rsockets, the socket API of librdmacm that the stream above is built on: sockets with Bind, Listen, Accept, Connect, Read, Write and Shutdown, their options, and Poll over rsockets and plain descriptors.
- [`verbs`](https://pkg.go.dev/github.com/wzshiming/rdma/verbs) binds libibverbs: devices, protection domains, memory regions, completion queues and channels, queue pairs, work requests and completions, with every wait on the Go runtime poller.
- [`cm`](https://pkg.go.dev/github.com/wzshiming/rdma/cm) binds librdmacm: resolving addresses, connecting and accepting RC connections with private data, and the events of a connection's life.

`verbs` and `cm` expose their rdma-core handles through `Pointer` methods and `*FromPointer` constructors, so they interoperate with other code built on rdma-core.

## Command line

`rdma` lists devices and measures streams; `rdma help <command>` prints the flags of a command. Its exit status is 0 on success, 1 when a command fails and 2 for a command line that makes no sense.

`rdma devices` lists every device with its GUID, ports and GID table through `verbs`:

```console
$ rdma devices
device: rxe0
  guid: 8c96aafffe58999d
  port: 1
    state: ACTIVE
    link layer: Ethernet
    mtu: 1024
    gid 0: fe80::8c96:aaff:fe58:999d (RoCE v2)
    gid 1: ::ffff:10.200.0.1 (RoCE v2)
```

`rdma pingpong` measures round-trip latency: the server echoes, the client times `-n` round trips of `-size` bytes and reports nearest-rank percentiles. The server prints the address it listens on, so port 0 works:

```console
$ rdma pingpong -listen 10.200.0.1:4791 &
listening on 10.200.0.1:4791
$ rdma pingpong -connect 10.200.0.1:4791 -n 1000 -size 64
size: 64
iterations: 1000
min: 24.204µs
median: 67.912µs
p99: 806.728µs
max: 1.877701ms
msg/s: 10417.54
```

`rdma bw` measures throughput: the client streams `-size` bytes at a time over `-conns` connections for `-duration`, the server discards what arrives and confirms the end of each stream:

```console
$ rdma bw -listen 10.200.0.1:4791 &
listening on 10.200.0.1:4791
$ rdma bw -connect 10.200.0.1:4791 -duration 2s
connections: 1
size: 65536
bytes: 1039925248
duration: 2.000346402s
MiB/s: 495.79
Gbit/s: 4.159
```

The samples above come from Soft-RoCE on a GitHub Actions runner; hardware is a different league.

`-sndbuf`, `-rcvbuf`, `-sqsize`, `-rqsize` and `-inline` set the rsockets options of the connections (`Config.SendBufferSize`, `Config.ReceiveBufferSize`, `Config.SendQueueSize`, `Config.ReceiveQueueSize` and `Config.MaxInline`; 0 keeps the rsockets default), and `-keep` makes a server serve run after run.

## Docker

The image `ghcr.io/wzshiming/rdma` holds the `rdma` command with the rdma-core libraries and providers, built for linux/amd64 and linux/arm64, and runs it as an unprivileged user. A push to master publishes `latest`, `master` and `sha-<short commit>`; a `v1.2.3` tag publishes `1.2.3`, `1.2` and `1`.

```sh
docker run --rm --network host --device /dev/infiniband --ulimit memlock=-1:-1 ghcr.io/wzshiming/rdma devices
docker run --rm --network host --device /dev/infiniband --ulimit memlock=-1:-1 ghcr.io/wzshiming/rdma pingpong -listen 10.200.0.1:4791 &
docker run --rm --network host --device /dev/infiniband --ulimit memlock=-1:-1 ghcr.io/wzshiming/rdma pingpong -connect 10.200.0.1:4791
docker run --rm --network host --device /dev/infiniband --ulimit memlock=-1:-1 ghcr.io/wzshiming/rdma bw -listen 10.200.0.1:4791 &
docker run --rm --network host --device /dev/infiniband --ulimit memlock=-1:-1 ghcr.io/wzshiming/rdma bw -connect 10.200.0.1:4791 -duration 2s
```

`--device /dev/infiniband` hands the container the uverbs and rdma_cm device nodes, `--network host` lets rdma_cm resolve the host's IP addresses and GIDs, and the memlock limit makes room for the registered buffers, which are pinned.

Build the image from a checkout with `docker build -t rdma .`.

## Soft-RoCE

Soft-RoCE runs RDMA over any Ethernet interface with the `rdma_rxe` kernel module, so one Linux host is enough to try the package; the CI workflow does exactly this on a stock Ubuntu runner, where it also builds rdma-core from source, since the 50.0 that Ubuntu 24.04 ships falls under the caveat in [Requirements](#requirements). Give rxe a dummy interface with a private address of its own: rdma_cm picks the device by the local IP, and a hardware RDMA device on the same host would otherwise claim the address of a shared interface.

```sh
sudo apt-get install -y rdma-core ibverbs-utils rdmacm-utils libibverbs-dev librdmacm-dev ibverbs-providers iproute2
sudo apt-get install -y "linux-modules-extra-$(uname -r)"  # rdma_rxe.ko lives here on Ubuntu
sudo modprobe rdma_rxe

sudo ip link add rxe-dummy0 type dummy
sudo ip addr add 10.200.0.1/24 dev rxe-dummy0
sudo ip link set rxe-dummy0 up
sudo rdma link add rxe0 type rxe netdev rxe-dummy0

rdma link       # rxe0 on rxe-dummy0, state ACTIVE
ibv_devinfo     # rxe0 with its GIDs, ::ffff:10.200.0.1 among them
rping -s -a 10.200.0.1 -C 5 &
rping -c -a 10.200.0.1 -C 5 -v
```

With the device up, raise the locked-memory limit and run the echo example or the command against itself:

```sh
ulimit -l unlimited
go run ./examples/echo -listen 10.200.0.1:4791 &
go run ./examples/echo -connect 10.200.0.1:4791 -message "hello, rdma"
rdma pingpong -listen 10.200.0.1:4791 &
rdma pingpong -connect 10.200.0.1:4791
```

## Platforms

Linux with cgo is the real implementation, over librdmacm and libibverbs. Every other GOOS, and Linux built with `CGO_ENABLED=0`, compiles the same API, with `Dial`, `Listen` and the constructors of `rsocket`, `verbs` and `cm` returning an error that `errors.Is(err, errors.ErrUnsupported)` matches; a program using the package builds everywhere and fails only where it needs RDMA.

## License

[MIT](LICENSE)
