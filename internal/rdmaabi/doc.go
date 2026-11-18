// Package rdmaabi mirrors the rdma-core structures the bindings exchange
// with libibverbs and librdmacm, and the commands package internal/ucma
// writes to the kernel's rdma_cm device: the UcmaAbi types of
// rdma/rdma_cma_abi.h, the IbUverbs types of rdma/ib_user_verbs.h they
// nest, and the AF_IB address of infiniband/ib.h and the path records of
// infiniband/sa.h they carry.
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
// IbvDevice, IbvContext, IbvCq, IbvQp and IbvSrq mirror the leading fields of
// their C structures only: struct ibv_device up to transport_type, which is
// all the bindings read of it, and the others up to the pthread objects whose
// size depends on the architecture. libibverbs allocates them; Go code never
// does. IbvContextAbiCompat gives, for each architecture, the offset of the
// abi_compat member that follows the mutex of struct ibv_context, and
// VerbsContext mirrors the members of struct verbs_context that precede the
// struct ibv_context it ends with, at their distance from it.
//
// The tests pin the layout in testdata/layout.txt and compare it with the
// installed rdma-core headers through the C compiler of every architecture
// RDMA_TEST_LAYOUT_ARCHES names.
package rdmaabi
