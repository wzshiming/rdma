package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/wzshiming/rdma/verbs"
)

var devicesCommand = &command{
	name:    "devices",
	summary: "list RDMA devices, ports and GIDs",
	run:     runDevices,
}

const devicesUsage = `Usage: rdma devices

List the RDMA devices of the host: for each device its GUID and ports, for
each port its state, link layer and active MTU, and the populated entries of
its GID table with their index, GID and type. The output is indented
"key: value" lines, one device after another:

  device: rxe0
    guid: 0250b6fffe0a4d8c
    port: 1
      state: ACTIVE
      link layer: Ethernet
      mtu: 1024
      gid 1: ::ffff:10.200.0.1 (RoCE v2)

A host without RDMA devices gets the line "no RDMA devices".
`

func runDevices(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("devices")
	if err := parseFlags(fs, args, nil); err != nil {
		return reportParse(err, fs, devicesUsage, stdout, stderr)
	}
	if err := listDevices(stdout); err != nil {
		return fail(fs.Name(), err, stderr)
	}
	return exitOK
}

// listDevices writes every device with its ports and GID table to w.
func listDevices(w io.Writer) error {
	devs, err := verbs.Devices()
	// A kernel without the RDMA stack has no sysfs class to list, which
	// libibverbs reports as ENOSYS: no devices either.
	if err != nil && !errors.Is(err, errNoSys) {
		return err
	}
	if len(devs) == 0 {
		fmt.Fprintln(w, "no RDMA devices")
		return nil
	}
	for _, d := range devs {
		if err := listDevice(w, d); err != nil {
			return fmt.Errorf("%s: %w", d.Name(), err)
		}
	}
	return nil
}

func listDevice(w io.Writer, d *verbs.Device) error {
	fmt.Fprintf(w, "device: %s\n", d.Name())
	fmt.Fprintf(w, "  guid: %016x\n", d.GUID())
	ctx, err := d.Open()
	if err != nil {
		return err
	}
	defer ctx.Close()
	attr, err := ctx.QueryDevice()
	if err != nil {
		return err
	}
	for port := uint8(1); port <= attr.PhysPortCnt; port++ {
		pa, err := ctx.QueryPort(port)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  port: %d\n", port)
		fmt.Fprintf(w, "    state: %v\n", pa.State)
		fmt.Fprintf(w, "    link layer: %v\n", pa.LinkLayer)
		fmt.Fprintf(w, "    mtu: %v\n", pa.ActiveMTU)
		gids, err := ctx.GIDTable(port)
		if err != nil {
			return err
		}
		for _, g := range gids {
			fmt.Fprintf(w, "    gid %d: %v (%v)\n", g.Index, g.GID, g.Type)
		}
	}
	return nil
}
