// Package rsproto is the stream protocol of rsockets, the socket layer of
// librdmacm, as librdmacm/rsocket.c of rdma-core v66.0 speaks it over
// InfiniBand and RoCE, byte for byte, so that the peer may be any rsockets
// program.
//
// Three formats travel on the wire: the message word, the immediate data of
// every message; the grant entry, which a receiver writes into the sender's
// grant table to give it room in its receive buffer; and the connection
// data, which each side puts in the private data of the connection request
// and reply. Direct I/O (riomap), the iWARP mode and datagrams are left out.
package rsproto
