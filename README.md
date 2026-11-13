# rdma

[![Go Reference](https://pkg.go.dev/badge/github.com/wzshiming/rdma.svg)](https://pkg.go.dev/github.com/wzshiming/rdma)
[![Go](https://github.com/wzshiming/rdma/actions/workflows/go.yml/badge.svg)](https://github.com/wzshiming/rdma/actions/workflows/go.yml)

Reliable byte streams over RDMA with the vocabulary of the `net` package: `Dial` and `Listen` give out `net.Conn` and `net.Listener` values addressed by IP address and port like TCP. The package is a Go implementation of rsockets, the stream protocol of librdmacm, on the module's own bindings of libibverbs and of the RDMA connection manager, `verbs` and `cm`: it runs over InfiniBand and RoCE and lends its registered buffers to `io.Copy`. On the wire it is stock rsockets, so any rsockets application can be its peer; the tests hold it to that against librdmacm's own rsockets and rstream, which are also what the [benchmarks](#benchmarks) measure it against. The module depends on the standard library, [`github.com/ebitengine/purego`](https://github.com/ebitengine/purego) and rdma-core, which the bindings load at run time.

## Requirements

- Linux with the kernel's RDMA stack (`ib_uverbs`, `rdma_ucm`) and either an RDMA device or Soft-RoCE (the `rdma_rxe` kernel module) over any Ethernet interface; see [Soft-RoCE](#soft-roce) for a one-host setup.
- rdma-core at run time: the libraries `libibverbs1`, `librdmacm1t64` and `ibverbs-providers` (Debian and Ubuntu package names; the library is `librdmacm1` before Debian trixie and Ubuntu 24.04), of which the package calls libibverbs and the connection manager of librdmacm, not its rsockets. Building needs neither the headers nor a C compiler: the bindings open `librdmacm.so.1` and `libibverbs.so.1` on first use.
- A locked-memory limit that holds the registered buffers, which are pinned: about 256 KiB per connection with the rsockets defaults, so the usual 8 MiB limit holds a few dozen connections. `ulimit -l unlimited` raises it for a shell.
- For the tests and benchmarks, rdma-core 53 or later: they run librdmacm's rsockets as the package's peer and yardstick, and earlier releases, the 50.0 of Ubuntu 24.04 among them, have the rsockets `cm_svc_run` bug, a service thread polling a freed descriptor array once a fourth rsocket registers, which hangs connections or crashes the process. The package itself registers no rsocket, so the bug does not touch it.

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

The networks are `rdma`, `rdma4` and `rdma6`; the IP address selects the device through rdma_cm. `Dialer` and `ListenConfig` carry the options, including a `Config` with the rsockets knobs of a connection, and every error is a `*net.OpError` on net `rdma`, so `errors.Is` matches `syscall.ECONNREFUSED`, `os.ErrDeadlineExceeded` and `net.ErrClosed` as it does for TCP. The [package documentation](https://pkg.go.dev/github.com/wzshiming/rdma) has the rest.

A peer on stock rsockets, `rstream` or a program run under `LD_PRELOAD=librspreload.so`, cannot tell the package from librdmacm's rsockets, but this end of a connection differs from stock rsockets in a few points: a peer lost without closing reads as `syscall.ECONNRESET` once what arrived before is read, not as the end of the stream; when the peer has vanished, `Close` gives the posted sends at most a second rather than the retry timeout of the transport; zero `Config` fields take the compile-time defaults of rsockets, as the package does not read `/etc/rdma/rsocket`, whose files (`mem_default`, `wmem_default`, `sqsize_default`, `rqsize_default`, `inline_default`) set the defaults of stock rsockets, the peer's among them; a call that has to wait polls first only for `Config.PollTime`, which [Command line](#command-line) explains with `-poll`; and iWARP devices are refused, as [Platforms](#platforms) tells.

`*Conn` implements `io.ReaderFrom` and `io.WriterTo`, so `io.Copy` moves the bytes without a buffer of its own in either direction, and without the copy such a buffer costs: `io.Copy(conn, src)` has `src` read straight into the registered send buffer the bytes go out from, and `io.Copy(dst, conn)` hands `dst` the registered receive buffer they arrived in, whose room returns to the peer only once `dst` has taken them, so what a failing `dst` leaves stays to be read. This takes the `*Conn` itself, as a wrapper such as a `bufio.Writer` or an `io.LimitReader` hides the methods from `io.Copy`, and `io.Copy(conn, src)` reaches `ReadFrom` only when `src` does not write itself out: `io.Copy` runs the `WriteTo` of a source that has one, which for an in-memory reader such as a `*bytes.Reader` is one `Write` of what it holds, while that of an `*os.File` comes back to `ReadFrom`.

## Packages

- [`verbs`](https://pkg.go.dev/github.com/wzshiming/rdma/verbs) binds libibverbs: devices, protection domains, memory regions, completion queues and channels, queue pairs, work requests and completions, with every wait on the Go runtime poller.
- [`cm`](https://pkg.go.dev/github.com/wzshiming/rdma/cm) binds librdmacm: resolving addresses, connecting and accepting RC connections with private data, and the events of a connection's life.

`verbs` and `cm`, on which the package runs, expose their rdma-core handles through `Pointer` methods and `*FromPointer` constructors, so they interoperate with other code built on rdma-core.

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

`-sndbuf`, `-rcvbuf`, `-sqsize`, `-rqsize` and `-inline` set the rsockets options of the connections (`Config.SendBufferSize`, `Config.ReceiveBufferSize`, `Config.SendQueueSize`, `Config.ReceiveQueueSize` and `Config.MaxInline`; 0 keeps the rsockets default); `-poll` sets how long a call that has to wait polls for completions before it sleeps (`Config.PollTime`; left out or 0, it does not poll, and `-poll 10us` polls for the 10 µs that rsockets poll by default, their `polling_time`); and `-keep` makes a server serve run after run.

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

Soft-RoCE runs RDMA over any Ethernet interface with the `rdma_rxe` kernel module, so one Linux host is enough to try the package; the CI workflow does exactly this on a stock Ubuntu runner, where it also builds rdma-core from source, as the tests need a later release than the 50.0 that Ubuntu 24.04 ships (see [Requirements](#requirements)). Give rxe a dummy interface with a private address of its own: rdma_cm picks the device by the local IP, and a hardware RDMA device on the same host would otherwise claim the address of a shared interface.

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

The root package has Go benchmarks that measure the package against the C implementation of rsockets on one device, in up to three columns, the sub-benchmarks `engine=rstream`, `engine=librdmacm` and `engine=native`: rstream, the test program of librdmacm, runs in C, a server and a client in blocking mode with the buffers of the row; librdmacm drives the C library's rsockets from Go, through the module's internal binding of them, which exists for the tests and benchmarks alone; and native is this package. `BenchmarkRXERoundTrip` times the round trip of 64 B and of 4 KiB, a `Write` that the peer reads and writes back and a `ReadFull` of the echo; `BenchmarkRXEWriteRead` measures the throughput one way of a loop of `Write` calls against a loop of `Read` calls, at 64 B, 4 KiB, 64 KiB and 1 MiB a call and at 1 MiB between connections of 1 MiB buffers, and `BenchmarkRXEReadFromWriteTo` that of `ReadFrom` on the sending end against `WriteTo` on the receiving one, at 4 KiB, 64 KiB and 1 MiB and with the 1 MiB buffers, failing unless the copy takes the path of its column: the lent registered buffers on native, a buffer of its own on librdmacm. `BenchmarkRXEDuplex` writes 64 KiB both ways at once, on native alone, as librdmacm's duplex at times wedged the process on Soft-RoCE; `BenchmarkRXEConcurrentRoundTrip` makes round trips on two connections per CPU; `BenchmarkRXEConnect` dials, accepts and closes a connection; `BenchmarkRXEPolledRoundTrip` and `BenchmarkRXEPolledWriteRead` repeat the round trips and the 64 B stream on native alone, its waits polling for 10 µs (`Config.PollTime`), the `polling_time` of rsockets, before they sleep. The other rows run on librdmacm and native, the round trips and the write-read streams on rstream as well. Next to the time and the throughput of an op, librdmacm and native report its allocations, its read and write class system calls, its CPU time and its voluntary context switches. The benchmarks find their device as the tests do, `RDMA_TEST_DEVICE` naming one, and skip without one.

`hack/benchcompare.sh` runs the comparison of a host. It builds the root package of the checkout it belongs to, or of the tree given last, runs ten rounds of the benchmarks, or `-rounds`, and has benchstat, at the version `go.mod` pins as a tool, set the samples side by side in the table "Implementations": rstream, librdmacm and native on that tree, librdmacm and native set against rstream, the base, in time and throughput, and native against librdmacm in what rstream does not report. On a host with a device and rstream on `PATH`, from a checkout:

```sh
ulimit -l unlimited
RDMA_TEST_DEVICE=rxe0 hack/benchcompare.sh
```

Given a baseline commit, each round first runs native on the baseline, on the tree and on the baseline again, and two more tables follow, of the benchmarks both trees have: "Noise", the baseline after the tree against the baseline before it, and "Change", the tree against the baseline, both of its groups; a baseline with the files of the tree leaves the implementations alone.

```sh
RDMA_TEST_DEVICE=rxe0 hack/benchcompare.sh -base master
```

The script prints the summary, the tables under a header of the trees and the host, and writes it to `summary.md` in a new temporary directory, or the one `-out` names, next to the output of each group's passes, `engines.txt`, `base-1.txt`, `test.txt` and `base-2.txt`, and to `profile.txt` and `cpu.pprof` of a pass that profiles native on the tree before the rounds. It sets `RDMA_TEST_REQUIRE=1`, so that a benchmark that would skip, without the device or rstream, fails instead, and a pass that fails, runs no benchmark or outlasts `-timeout`, 300 seconds by default, ends the comparison with its output and without a summary. The rows of 1 MiB buffers pin about 8 MiB a pair of connections, which the locked-memory limit has to hold.

Every run of the [Go workflow](https://github.com/wzshiming/rdma/actions/workflows/go.yml) runs the script on Soft-RoCE in its integration job, after the tests, in the step "Compare the benchmarks": a push to an existing branch against the tip of the branch before the push, a new branch, a pull request and a forced push whose previous tip is gone against the merge base with master. The run's Summary page shows the tables under "Benchmarks on rxe0", the log of the step repeats them and names the baseline, and the artifact `benchmark-comparison` holds what the script wrote. The figures never fail the job; a benchmark that fails does. Soft-RoCE is a software device and the hosted runners vary in their CPUs, so the figures count only relative to each other within one run; [run 43405248113](https://github.com/wzshiming/rdma/actions/runs/43405248113), on 4 CPUs of an AMD EPYC 7763, gave, to three significant digits:

| Benchmark | rstream | librdmacm | native |
| --- | --- | --- | --- |
| Round trip, 64 B | 38.5 µs | 216 µs | 114 µs |
| Round trip, 4 KiB | 70.2 µs | 296 µs | 141 µs |
| `Write` / `Read`, 64 B | 14.5 MiB/s | 1.44 MiB/s | 17.2 MiB/s |
| `Write` / `Read`, 4 KiB | 203 MiB/s | 84.3 MiB/s | 237 MiB/s |
| `Write` / `Read`, 64 KiB | 252 MiB/s | 210 MiB/s | 248 MiB/s |
| `Write` / `Read`, 1 MiB | 247 MiB/s | 212 MiB/s | 247 MiB/s |
| `ReadFrom` / `WriteTo`, 4 KiB | | 83.5 MiB/s | 235 MiB/s |
| `ReadFrom` / `WriteTo`, 1 MiB | | 200 MiB/s | 247 MiB/s |

In that run the package moved small writes faster than rstream, 19 percent more bytes a second at 64 B and 17 percent at 4 KiB, kept up with it at 64 KiB and 1 MiB, within 2 percent, and trailed in the round trips, which took it about three times as long at 64 B and twice as long at 4 KiB; it was ahead of librdmacm in every row.

Between two hosts, `rdma pingpong` and `rdma bw` of the [command line](#command-line) make the round trips and the stream of `Write` calls of the benchmarks, and rstream makes them in C: its server binds with `-b`, its client connects with `-s`, both block, `-T b`, and both take the same size (`-S`), transfers per iteration (`-C`) and iterations (`-I`), which rstream does not send its peer; `-B 65536` gives it the buffers of the zero `Config`, 128 KiB registered each way. Each server serves one client and exits, so the client's command follows each of the server's in turn:

```sh
# on the server, 10.0.0.1
rdma pingpong -listen 10.0.0.1:7471
rstream -b 10.0.0.1 -p 7471 -T b -B 65536 -S 64 -C 1 -I 100000
rdma bw -listen 10.0.0.1:7471
rstream -b 10.0.0.1 -p 7471 -T b -B 65536 -S 65536 -C 10000 -I 1
# on the client
rdma pingpong -connect 10.0.0.1:7471 -size 64 -n 100000
rstream -s 10.0.0.1 -p 7471 -T b -B 65536 -S 64 -C 1 -I 100000
rdma bw -connect 10.0.0.1:7471 -size 65536 -duration 10s
rstream -s 10.0.0.1 -p 7471 -T b -B 65536 -S 65536 -C 10000 -I 1
```

pingpong reports the round trip, rstream its `usec/xfer`, the time of a transfer one way, which is half a round trip in the first test; bw reports `Gbit/s` one way, rstream `Gb/sec` over its transfers to the server and then back. `-sndbuf 1048576 -rcvbuf 1048576` on both commands with `-B 1048576` for rstream run the buffers of the 1 MiB rows.

## Platforms

The real implementation is Linux on amd64, arm64, loong64, ppc64le, riscv64 and s390x, with or without cgo: the bindings load `librdmacm.so.1` and `libibverbs.so.1` through [`purego`](https://github.com/ebitengine/purego) on first use and call them directly, so `CGO_ENABLED` changes nothing, and a host without the libraries gets an error that `errors.Is(err, errors.ErrUnsupported)` matches from `Dial`, `Listen` and the constructors of `verbs` and `cm` rather than a program that cannot start. A build without cgo links glibc's dynamic loader; musl is unverified. The architectures are verified to different degrees: amd64 runs the device tests on the Soft-RoCE device of the CI workflow, arm64 runs the tests on a host without a device, and loong64, ppc64le, riscv64 and s390x get a cross vet and the compile-time check of the structure layouts against the rdma-core headers, so no code runs on them in CI. Every other GOOS and GOARCH, linux/386 and linux/arm among them, compiles the same API with those constructors returning the same error; a program using the package builds everywhere and fails only where it needs RDMA.

Of the RDMA transports, the package runs on InfiniBand and RoCE but not on iWARP: over an iWARP device a dial fails with an error that `errors.Is(err, errors.ErrUnsupported)` matches, and a listener turns the requests away.

## License

[MIT](LICENSE)
