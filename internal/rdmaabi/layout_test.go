package rdmaabi

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/layout.txt from the Go types")

// A mirror pairs a Go type with the C type whose layout it reproduces.
type mirror struct {
	c string       // the C type, e.g. "struct ibv_wc"
	t reflect.Type // the Go type
	// prefix marks a type that mirrors the leading fields of its C type
	// only; the library allocates these, never Go code.
	prefix bool
}

var mirrors = []mirror{
	{c: "struct ibv_device", t: reflect.TypeFor[IbvDevice](), prefix: true},
	{c: "struct ibv_device_attr", t: reflect.TypeFor[IbvDeviceAttr]()},
	{c: "struct ibv_port_attr", t: reflect.TypeFor[IbvPortAttr]()},
	{c: "struct ibv_gid_entry", t: reflect.TypeFor[IbvGidEntry]()},
	{c: "union ibv_gid", t: reflect.TypeFor[IbvGid]()},
	{c: "struct ibv_context_ops", t: reflect.TypeFor[IbvContextOps]()},
	{c: "struct ibv_context", t: reflect.TypeFor[IbvContext](), prefix: true},
	{c: "struct ibv_comp_channel", t: reflect.TypeFor[IbvCompChannel]()},
	{c: "struct ibv_async_event", t: reflect.TypeFor[IbvAsyncEvent]()},
	{c: "struct ibv_wc", t: reflect.TypeFor[IbvWc]()},
	{c: "struct ibv_mr", t: reflect.TypeFor[IbvMr]()},
	{c: "struct ibv_cq", t: reflect.TypeFor[IbvCq](), prefix: true},
	{c: "struct ibv_qp", t: reflect.TypeFor[IbvQp](), prefix: true},
	{c: "struct ibv_qp_init_attr", t: reflect.TypeFor[IbvQpInitAttr]()},
	{c: "struct ibv_qp_cap", t: reflect.TypeFor[IbvQpCap]()},
	{c: "struct ibv_qp_attr", t: reflect.TypeFor[IbvQpAttr]()},
	{c: "struct ibv_ah_attr", t: reflect.TypeFor[IbvAhAttr]()},
	{c: "struct ibv_global_route", t: reflect.TypeFor[IbvGlobalRoute]()},
	{c: "struct ibv_send_wr", t: reflect.TypeFor[IbvSendWr]()},
	{c: "struct ibv_recv_wr", t: reflect.TypeFor[IbvRecvWr]()},
	{c: "struct ibv_sge", t: reflect.TypeFor[IbvSge]()},
	{c: "struct rdma_event_channel", t: reflect.TypeFor[RdmaEventChannel]()},
	{c: "struct rdma_cm_id", t: reflect.TypeFor[RdmaCmId]()},
	{c: "struct rdma_route", t: reflect.TypeFor[RdmaRoute]()},
	{c: "struct rdma_addr", t: reflect.TypeFor[RdmaAddr]()},
	{c: "struct rdma_ib_addr", t: reflect.TypeFor[RdmaIbAddr]()},
	{c: "struct rdma_cm_event", t: reflect.TypeFor[RdmaCmEvent]()},
	{c: "struct rdma_conn_param", t: reflect.TypeFor[RdmaConnParam]()},
}

// A field is a mirrored field under its C member designator, such as
// "wr.rdma.remote_addr", with its offset and size in the Go type.
type field struct {
	name   string
	offset uintptr
	size   uintptr
}

// fields lists the named fields of t in declaration order, anonymous nested
// structs flattened into their members and padding (_) left out.
func fields(t reflect.Type) []field {
	var out []field
	var walk func(t reflect.Type, prefix string, base uintptr)
	walk = func(t reflect.Type, prefix string, base uintptr) {
		for i := range t.NumField() {
			f := t.Field(i)
			if f.Name == "_" {
				continue
			}
			name := prefix + cName(f.Name)
			if f.Type.Kind() == reflect.Struct && f.Type.Name() == "" {
				walk(f.Type, name+".", base+f.Offset)
				continue
			}
			out = append(out, field{name, base + f.Offset, f.Type.Size()})
		}
	}
	if t.Kind() == reflect.Struct {
		walk(t, "", 0)
	}
	return out
}

// cName turns a Go field name back into the C member name it mirrors.
func cName(goName string) string {
	return strings.ToLower(goName[:1]) + goName[1:]
}

// layoutText renders the size of every mirror and the offset and size of
// every field, the content of testdata/layout.txt.
func layoutText() string {
	var b strings.Builder
	b.WriteString("# Layout of the rdma-core mirrors in bytes: \"<C type> <size>\" and\n")
	b.WriteString("# \"<C type>.<member> <offset> <size>\". Regenerate with go test -update\n")
	b.WriteString("# once TestLayoutsMatchC passes.\n")
	for _, m := range mirrors {
		fmt.Fprintf(&b, "%s %d\n", m.c, m.t.Size())
		for _, f := range fields(m.t) {
			fmt.Fprintf(&b, "%s.%s %d %d\n", m.c, f.name, f.offset, f.size)
		}
	}
	return b.String()
}

// parseLayout maps each line of a layout text, "struct ibv_wc.wr_id 0 8",
// from its subject, "struct ibv_wc.wr_id", to its numbers, "0 8".
func parseLayout(text string) map[string]string {
	m := make(map[string]string)
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words := strings.Fields(line)
		m[strings.Join(words[:2], " ")] = strings.Join(words[2:], " ")
	}
	return m
}

// TestLayoutMatchesBaseline checks the size of every type and the offset of
// every field against testdata/layout.txt on whatever platform runs it.
func TestLayoutMatchesBaseline(t *testing.T) {
	path := filepath.Join("testdata", "layout.txt")
	text := layoutText()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, want := parseLayout(text), parseLayout(string(baseline))
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if got[k] != want[k] {
			t.Errorf("%s: Go %q, baseline %q", k, got[k], want[k])
		}
	}
}

// TestTypesHoldOnlyData checks that the mirrors are plain data the library
// can be handed: no methods, no Go pointers, every scalar of a fixed size.
func TestTypesHoldOnlyData(t *testing.T) {
	for _, m := range mirrors {
		if n := m.t.NumMethod() + reflect.PointerTo(m.t).NumMethod(); n != 0 {
			t.Errorf("%s has %d methods", m.t, n)
		}
		checkData(t, m.t.String(), m.t)
	}
}

func checkData(t *testing.T, path string, typ reflect.Type) {
	switch typ.Kind() {
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
	case reflect.Array:
		checkData(t, path+"[]", typ.Elem())
	case reflect.Struct:
		for i := range typ.NumField() {
			f := typ.Field(i)
			checkData(t, path+"."+f.Name, f.Type)
		}
	default:
		t.Errorf("%s: %s is not fixed-size data", path, typ.Kind())
	}
}
