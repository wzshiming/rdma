# rdma

[![Go Reference](https://pkg.go.dev/badge/github.com/wzshiming/rdma.svg)](https://pkg.go.dev/github.com/wzshiming/rdma)
[![Go](https://github.com/wzshiming/rdma/actions/workflows/go.yml/badge.svg)](https://github.com/wzshiming/rdma/actions/workflows/go.yml)

Reliable byte streams over RDMA with the vocabulary of the `net` package: `Dial` and `Listen` give out `net.Conn` and `net.Listener` values backed by an RC queue pair, addressed by IP address and port like TCP, working over InfiniBand, RoCE and iWARP alike, and depending on nothing but the standard library and rdma-core.

## Requirements

- Linux with the kernel's RDMA stack (`ib_uverbs`, `rdma_ucm`) and either an RDMA device or Soft-RoCE (the `rdma_rxe` kernel module) over any Ethernet interface; see [Soft-RoCE](#soft-roce) for a one-host setup.
- rdma-core: the headers `libibverbs-dev` and `librdmacm-dev` to build, the libraries `libibverbs1`, `librdmacm1` and `ibverbs-providers` to run (Debian and Ubuntu package names).
- A locked-memory limit that holds the registered buffers, which are pinned: about 1 MiB per connection with the defaults, so the usual 8 MiB limit holds a handful of connections. `ulimit -l unlimited` raises it for a shell.

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

The networks are `rdma`, `rdma4` and `rdma6`; the IP address selects the device through rdma_cm. `Dialer` and `ListenConfig` carry the options, including a `Config` that sizes the buffers of a connection, and every error is a `*net.OpError` on net `rdma`, so `errors.Is` matches `syscall.ECONNREFUSED`, `os.ErrDeadlineExceeded` and `net.ErrClosed` as it does for TCP. The [package documentation](https://pkg.go.dev/github.com/wzshiming/rdma) has the rest.

## Packages

- [`verbs`](https://pkg.go.dev/github.com/wzshiming/rdma/verbs) binds libibverbs: devices, protection domains, memory regions, completion queues and channels, queue pairs, work requests and completions, with every wait on the Go runtime poller.
- [`cm`](https://pkg.go.dev/github.com/wzshiming/rdma/cm) binds librdmacm: resolving addresses, connecting and accepting RC connections with private data, and the events of a connection's life.

Both expose their rdma-core handles through `Pointer` methods and `*FromPointer` constructors, so they interoperate with other code built on rdma-core.

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

`-msgsize` and `-depth` set the message size and queue depth of the connections (`Config.MessageSize`, `Config.ReceiveDepth` and `Config.SendDepth`), and `-keep` makes a server serve run after run.

## Soft-RoCE

Soft-RoCE runs RDMA over any Ethernet interface with the `rdma_rxe` kernel module, so one Linux host is enough to try the package; the CI workflow does exactly this on a stock Ubuntu runner. Give rxe a dummy interface with a private address of its own: rdma_cm picks the device by the local IP, and a hardware RDMA device on the same host would otherwise claim the address of a shared interface.

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

Linux with cgo is the real implementation, over libibverbs and librdmacm. Every other GOOS, and Linux built with `CGO_ENABLED=0`, compiles the same API, with `Dial`, `Listen` and the constructors of `verbs` and `cm` returning an error that `errors.Is(err, errors.ErrUnsupported)` matches; a program using the package builds everywhere and fails only where it needs RDMA.

## License

[MIT](LICENSE)
