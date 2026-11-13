# rdma

[![Go Reference](https://pkg.go.dev/badge/github.com/wzshiming/rdma.svg)](https://pkg.go.dev/github.com/wzshiming/rdma)
[![Go](https://github.com/wzshiming/rdma/actions/workflows/go.yml/badge.svg)](https://github.com/wzshiming/rdma/actions/workflows/go.yml)

Reliable byte streams over RDMA with the vocabulary of the `net` package: `Dial` and `Listen` give out `net.Conn` and `net.Listener` values addressed by IP address and port like TCP. The connections speak the rsockets protocol, so any rsockets application can be their peer, and one of two engines carries them, as `Config.Engine` picks: the native engine, the default, runs the protocol in Go on the module's own `verbs` and `cm` bindings, over InfiniBand and RoCE, and lends its registered buffers to `io.Copy`; the librdmacm engine drives the rsockets of librdmacm, iWARP included, and stays at hand as the compatibility peer and the control of the [benchmarks](#benchmarks). The module depends on the standard library, [`github.com/ebitengine/purego`](https://github.com/ebitengine/purego) and rdma-core, which the bindings load at run time.

## Requirements

- Linux with the kernel's RDMA stack (`ib_uverbs`, `rdma_ucm`) and either an RDMA device or Soft-RoCE (the `rdma_rxe` kernel module) over any Ethernet interface; see [Soft-RoCE](#soft-roce) for a one-host setup.
- rdma-core at run time, whichever the engine: the libraries `libibverbs1`, `librdmacm1t64` and `ibverbs-providers` (Debian and Ubuntu package names; the library is `librdmacm1` before Debian trixie and Ubuntu 24.04), of which the native engine calls libibverbs and the connection manager of librdmacm, and the librdmacm engine its rsockets. Building needs neither the headers nor a C compiler: the bindings open `librdmacm.so.1` and `libibverbs.so.1` on first use.
- A locked-memory limit that holds the registered buffers, which are pinned: about 256 KiB per connection with the rsockets defaults, so the usual 8 MiB limit holds a few dozen connections. `ulimit -l unlimited` raises it for a shell.
- For the librdmacm engine, two things of librdmacm's rsockets: rdma-core 53 or later to hold more than three connections in one process reliably, as earlier releases, the 50.0 of Ubuntu 24.04 among them, have the rsockets `cm_svc_run` bug, a service thread polling a freed descriptor array once a fourth rsocket registers, which hangs connections or crashes the process; and `/etc/rdma/rsocket`, whose files (`mem_default`, `wmem_default`, `sqsize_default`, `rqsize_default`, `inline_default`) set the defaults of the whole process that zero `Config` fields keep. The native engine has neither the bug nor the files: its zero fields take the compile-time defaults of rsockets.

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

The networks are `rdma`, `rdma4` and `rdma6`; the IP address selects the device through rdma_cm. `Dialer` and `ListenConfig` carry the options, including a `Config` with the engine and the rsockets knobs of a connection, and every error is a `*net.OpError` on net `rdma`, so `errors.Is` matches `syscall.ECONNREFUSED`, `os.ErrDeadlineExceeded` and `net.ErrClosed` as it does for TCP. The [package documentation](https://pkg.go.dev/github.com/wzshiming/rdma) has the rest.

`Config.Engine` picks the engine, `rdma.EngineNative`, which the zero value means too, or `rdma.EngineLibrdmacm`, and a listener accepts connections of its own engine, whatever the dialer runs. Either engine speaks the rsockets protocol itself, so the peer may be any rsockets application, `rstream` or a program run under `LD_PRELOAD=librspreload.so` included, and cannot tell which engine it talks to. They differ at the edges: a peer lost without closing reads as `syscall.ECONNRESET` on the native engine, once what arrived before is read, and as `io.EOF` on the librdmacm engine, as rsockets report it; when the peer has vanished, `Close` gives the posted sends at most a second on the native engine and the transport's retry timeout, several seconds, on the librdmacm engine; and only the librdmacm engine reads `/etc/rdma/rsocket` and runs on iWARP, as [Requirements](#requirements) and [Platforms](#platforms) tell.

`*Conn` implements `io.ReaderFrom` and `io.WriterTo`, so `io.Copy` moves the bytes without a buffer of its own in either direction, and on the native engine without the copy such a buffer costs: `io.Copy(conn, src)` has `src` read straight into the registered send buffer the bytes go out from, and `io.Copy(dst, conn)` hands `dst` the registered receive buffer they arrived in, whose room returns to the peer only once `dst` has taken them, so what a failing `dst` leaves stays to be read. This takes the `*Conn` itself, as a wrapper such as a `bufio.Writer` or an `io.LimitReader` hides the methods from `io.Copy`, and `io.Copy(conn, src)` reaches `ReadFrom` only when `src` does not write itself out: `io.Copy` runs the `WriteTo` of a source that has one, which for an in-memory reader such as a `*bytes.Reader` is one `Write` of what it holds, while that of an `*os.File` comes back to `ReadFrom`. The librdmacm engine lends nothing, as rsockets keep their buffers to themselves, so on it both methods move the bytes through a buffer of their own, as `io.Copy` would.

## Packages

- [`rsocket`](https://pkg.go.dev/github.com/wzshiming/rdma/rsocket) binds rsockets, the socket API of librdmacm that the librdmacm engine drives: sockets with Bind, Listen, Accept, Connect, Read, Write and Shutdown, their options, and Poll over rsockets and plain descriptors.
- [`verbs`](https://pkg.go.dev/github.com/wzshiming/rdma/verbs) binds libibverbs: devices, protection domains, memory regions, completion queues and channels, queue pairs, work requests and completions, with every wait on the Go runtime poller.
- [`cm`](https://pkg.go.dev/github.com/wzshiming/rdma/cm) binds librdmacm: resolving addresses, connecting and accepting RC connections with private data, and the events of a connection's life.

`verbs` and `cm`, which the native engine runs on, expose their rdma-core handles through `Pointer` methods and `*FromPointer` constructors, so they interoperate with other code built on rdma-core.

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

`-engine` picks the engine of the connections, `native` or `librdmacm` (`Config.Engine`; left out, the library's default, the native engine), and the two ends need not pick the same; `-sndbuf`, `-rcvbuf`, `-sqsize`, `-rqsize` and `-inline` set the rsockets options of the connections (`Config.SendBufferSize`, `Config.ReceiveBufferSize`, `Config.SendQueueSize`, `Config.ReceiveQueueSize` and `Config.MaxInline`; 0 keeps the rsockets default), and `-keep` makes a server serve run after run.

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

Soft-RoCE runs RDMA over any Ethernet interface with the `rdma_rxe` kernel module, so one Linux host is enough to try the package; the CI workflow does exactly this on a stock Ubuntu runner, where it also builds rdma-core from source, since the 50.0 that Ubuntu 24.04 ships falls under the librdmacm engine's caveat in [Requirements](#requirements). Give rxe a dummy interface with a private address of its own: rdma_cm picks the device by the local IP, and a hardware RDMA device on the same host would otherwise claim the address of a shared interface.

```sh
sudo apt-get install -y rdma-core ibverbs-utils rdmacm-utils ibverbs-providers iproute2
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

## Benchmarks

The root package has Go benchmarks that put the two engines side by side on one device: `BenchmarkRXERoundTrip` times the round trip of 64 B and of 4 KiB, a `Write` that the peer reads and writes back and a `ReadFull` of the echo; `BenchmarkRXEWriteRead` measures the throughput one way of a loop of `Write` calls against a loop of `Read` calls, and `BenchmarkRXEReadFromWriteTo` that of `ReadFrom` on the sending end against `WriteTo` on the receiving one, both at 4 KiB, 64 KiB and 1 MiB a call, the latter failing unless the copy takes the path of its engine: the lent registered buffers on the native engine, a buffer of its own on the librdmacm engine. Each size runs on `engine=librdmacm` and then on `engine=native`. The benchmarks find their device as the tests do, `RDMA_TEST_DEVICE` naming one, and skip without one.

Ten rounds of every benchmark, in which the engines take turns on the machine, make the samples, and benchstat, at the version `go.mod` pins as a tool, compares them: a row per benchmark and size, a column per engine, the native one set against librdmacm's, the base. From a checkout, on a host with a device:

```sh
ulimit -l unlimited
for i in $(seq 10); do go test -run '^$' -bench '^BenchmarkRXE' -benchtime 500ms -count 1 .; done > bench.txt
go tool benchstat -col /engine bench.txt
```

Every run of the [Go workflow](https://github.com/wzshiming/rdma/actions/workflows/go.yml) does the same on Soft-RoCE in its integration job, after the tests: the run's Summary page shows the table under "Engines on rxe0", with the runner's CPU, kernel, Go and rdma-core versions and its load before and after, the log of the step "Summarize the benchmarks" repeats it, and the artifact `engine-benchmarks` holds the raw output. The figures never fail the job; a benchmark that fails does. Soft-RoCE is a software device, so only the relative differences mean anything; for orientation, one run on 4 CPUs of an AMD EPYC 9V45 gave:

| Benchmark | librdmacm | native |
| --- | --- | --- |
| Round trip, 64 B | 95 µs | 63 µs |
| `Write` / `Read`, 4 KiB | 139 MiB/s | 338 MiB/s |
| `Write` / `Read`, 64 KiB | 329 MiB/s | 358 MiB/s |
| `ReadFrom` / `WriteTo`, 4 KiB | 142 MiB/s | 340 MiB/s |
| `ReadFrom` / `WriteTo`, 1 MiB | 328 MiB/s | 358 MiB/s |

## Platforms

The real implementation is Linux on amd64, arm64, loong64, ppc64le, riscv64 and s390x, with or without cgo: the bindings load `librdmacm.so.1` and `libibverbs.so.1` through [`purego`](https://github.com/ebitengine/purego) on first use and call them directly, so `CGO_ENABLED` changes nothing, and a host without the libraries gets an error that `errors.Is(err, errors.ErrUnsupported)` matches from `Dial`, `Listen` and the constructors of `rsocket`, `verbs` and `cm` rather than a program that cannot start. A build without cgo links glibc's dynamic loader; musl is unverified. The architectures are verified to different degrees: amd64 runs the device tests of both engines on the Soft-RoCE device of the CI workflow, arm64 runs the tests on a host without a device, and loong64, ppc64le, riscv64 and s390x get a cross vet and the compile-time check of the structure layouts against the rdma-core headers, so no code runs on them in CI. Every other GOOS and GOARCH, linux/386 and linux/arm among them, compiles the same API with those constructors returning the same error; a program using the package builds everywhere and fails only where it needs RDMA.

Of the RDMA transports, the native engine runs on InfiniBand and RoCE, and iWARP devices take the librdmacm engine, `rdma.EngineLibrdmacm` in `Config.Engine` or `-engine librdmacm` on the command line: over an iWARP device a native dial fails with an error that `errors.Is(err, errors.ErrUnsupported)` matches and that names the librdmacm engine, and a native listener turns the requests away.

## License

Licensed under the MIT License. See [LICENSE](https://github.com/wzshiming/rdma/blob/master/LICENSE) for the full license text.
