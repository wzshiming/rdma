// Package rdmaabi mirrors the rdma-core structures the bindings exchange
// with libibverbs and librdmacm.
//
// Each type reproduces the layout of its C counterpart field by field: the
// same offsets and sizes on every supported Linux architecture (amd64,
// arm64, loong64, ppc64le, riscv64 and s390x), which share the LP64 data
// model and the same alignment rules, so one definition serves them all.
// The types hold data only and contain no Go pointers: C pointers are
// uint64, fields in network byte order (immediate data, GUIDs, GIDs) are
// byte arrays, host-order fields are fixed-size integers, and each field
// bears the C member name with an upper-case initial.
//
// IbvDevice, IbvContext, IbvCq and IbvQp mirror the leading fields of their
// C structures only: struct ibv_device up to transport_type, which is all the
// bindings read of it, and the other three up to the pthread objects whose
// size depends on the architecture. libibverbs allocates them; Go code never
// does.
//
// The tests pin the layout in testdata/layout.txt and compare it with the
// installed rdma-core headers through the C compiler of every architecture
// RDMA_TEST_LAYOUT_ARCHES names.
package rdmaabi
