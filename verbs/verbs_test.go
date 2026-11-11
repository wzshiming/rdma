package verbs_test

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

func TestEnumString(t *testing.T) {
	tests := []struct {
		v    fmt.Stringer
		want string
	}{
		{verbs.MTU256, "256"},
		{verbs.MTU1024, "1024"},
		{verbs.MTU4096, "4096"},
		{verbs.MTU(9), "MTU(9)"},
		{verbs.PortNop, "NOP"},
		{verbs.PortDown, "DOWN"},
		{verbs.PortActive, "ACTIVE"},
		{verbs.PortActiveDefer, "ACTIVE_DEFER"},
		{verbs.PortState(9), "PortState(9)"},
		{verbs.LinkLayerUnspecified, "Unspecified"},
		{verbs.LinkLayerInfiniBand, "InfiniBand"},
		{verbs.LinkLayerEthernet, "Ethernet"},
		{verbs.LinkLayer(3), "LinkLayer(3)"},
		{verbs.GIDTypeIB, "IB"},
		{verbs.GIDTypeRoCEv1, "RoCE v1"},
		{verbs.GIDTypeRoCEv2, "RoCE v2"},
		{verbs.GIDType(3), "GIDType(3)"},
		{verbs.TransportUnknown, "Unknown"},
		{verbs.TransportIB, "InfiniBand"},
		{verbs.TransportIWARP, "iWARP"},
		{verbs.TransportUSNIC, "usNIC"},
		{verbs.TransportUSNICUDP, "usNIC UDP"},
		{verbs.TransportUnspecified, "Unspecified"},
		{verbs.TransportType(5), "TransportType(5)"},
		{verbs.QPTypeRC, "RC"},
		{verbs.QPTypeUC, "UC"},
		{verbs.QPTypeUD, "UD"},
		{verbs.QPTypeRawPacket, "RAW_PACKET"},
		{verbs.QPTypeXRCSend, "XRC_SEND"},
		{verbs.QPTypeXRCRecv, "XRC_RECV"},
		{verbs.QPTypeDriver, "DRIVER"},
		{verbs.QPType(0), "QPType(0)"},
		{verbs.QPStateReset, "RESET"},
		{verbs.QPStateInit, "INIT"},
		{verbs.QPStateRTR, "RTR"},
		{verbs.QPStateRTS, "RTS"},
		{verbs.QPStateSQD, "SQD"},
		{verbs.QPStateSQE, "SQE"},
		{verbs.QPStateErr, "ERR"},
		{verbs.QPStateUnknown, "UNKNOWN"},
		{verbs.QPState(8), "QPState(8)"},
		{verbs.MigMigrated, "MIGRATED"},
		{verbs.MigRearm, "REARM"},
		{verbs.MigArmed, "ARMED"},
		{verbs.MigState(3), "MigState(3)"},
		{verbs.WRWrite, "RDMA_WRITE"},
		{verbs.WRWriteImm, "RDMA_WRITE_WITH_IMM"},
		{verbs.WRSend, "SEND"},
		{verbs.WRSendImm, "SEND_WITH_IMM"},
		{verbs.WRRead, "RDMA_READ"},
		{verbs.WROpcode(5), "WROpcode(5)"},
		{verbs.WCOpSend, "SEND"},
		{verbs.WCOpRDMAWrite, "RDMA_WRITE"},
		{verbs.WCOpRDMARead, "RDMA_READ"},
		{verbs.WCOpAtomicWrite, "ATOMIC_WRITE"},
		{verbs.WCOpRecv, "RECV"},
		{verbs.WCOpRecvRDMAWithImm, "RECV_RDMA_WITH_IMM"},
		{verbs.WCOpDriver3, "DRIVER3"},
		{verbs.WCOpcode(10), "WCOpcode(10)"},
		{verbs.WCSuccess, "success"},
		{verbs.WCLocLenErr, "local length error"},
		{verbs.WCLocProtErr, "local protection error"},
		{verbs.WCWRFlushErr, "Work Request Flushed Error"},
		{verbs.WCRemAccessErr, "remote access error"},
		{verbs.WCRetryExcErr, "transport retry counter exceeded"},
		{verbs.WCRNRRetryExcErr, "RNR retry counter exceeded"},
		{verbs.WCRemInvRDReqErr, "remote invalid RD request"},
		{verbs.WCTMRndvIncomplete, "TM software rendezvous"},
		{verbs.WCStatus(24), "WCStatus(24)"},
		{verbs.EventCQErr, "CQ_ERR"},
		{verbs.EventQPFatal, "QP_FATAL"},
		{verbs.EventSQDrained, "SQ_DRAINED"},
		{verbs.EventPortActive, "PORT_ACTIVE"},
		{verbs.EventGIDChange, "GID_CHANGE"},
		{verbs.EventWQFatal, "WQ_FATAL"},
		{verbs.EventType(20), "EventType(20)"},
		{verbs.AccessFlags(0), "0"},
		{verbs.AccessLocalWrite, "LOCAL_WRITE"},
		{verbs.AccessLocalWrite | verbs.AccessRemoteWrite | verbs.AccessRemoteRead, "LOCAL_WRITE|REMOTE_WRITE|REMOTE_READ"},
		{verbs.AccessRelaxedOrdering, "RELAXED_ORDERING"},
		{verbs.AccessLocalWrite | 1<<30, "LOCAL_WRITE|0x40000000"},
		{verbs.SendSignaled | verbs.SendInline, "SIGNALED|INLINE"},
		{verbs.SendFence | verbs.SendSolicited | verbs.SendIPCsum, "FENCE|SOLICITED|IP_CSUM"},
		{verbs.WCGRH | verbs.WCWithImm, "GRH|WITH_IMM"},
		{verbs.WCWithInv | verbs.WCIPCsumOK, "IP_CSUM_OK|WITH_INV"},
		{verbs.QPAttrState | verbs.QPAttrPkeyIndex | verbs.QPAttrPort | verbs.QPAttrAccessFlags, "STATE|ACCESS_FLAGS|PKEY_INDEX|PORT"},
		{verbs.QPAttrAV | verbs.QPAttrPathMTU | verbs.QPAttrDestQPN | verbs.QPAttrRQPSN | verbs.QPAttrMaxDestRDAtomic | verbs.QPAttrMinRNRTimer, "AV|PATH_MTU|RQ_PSN|MIN_RNR_TIMER|MAX_DEST_RD_ATOMIC|DEST_QPN"},
		{verbs.QPAttrTimeout | verbs.QPAttrRetryCnt | verbs.QPAttrRNRRetry | verbs.QPAttrSQPSN | verbs.QPAttrMaxQPRDAtomic | verbs.QPAttrRateLimit, "TIMEOUT|RETRY_CNT|RNR_RETRY|MAX_QP_RD_ATOMIC|SQ_PSN|RATE_LIMIT"},
		{verbs.GID{}, "::"},
		{verbs.GID{0: 0xfe, 1: 0x80, 15: 0x01}, "fe80::1"},
		{verbs.GID{10: 0xff, 11: 0xff, 12: 10, 13: 1, 14: 1, 15: 7}, "::ffff:10.1.1.7"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%T/%s", tt.v, tt.want), func(t *testing.T) {
			if got := tt.v.String(); got != tt.want {
				t.Errorf("%T(%v).String() = %q, want %q", tt.v, tt.v, got, tt.want)
			}
		})
	}
}

func TestMTUBytes(t *testing.T) {
	tests := []struct {
		mtu  verbs.MTU
		want int
	}{
		{verbs.MTU256, 256},
		{verbs.MTU512, 512},
		{verbs.MTU1024, 1024},
		{verbs.MTU2048, 2048},
		{verbs.MTU4096, 4096},
		{verbs.MTU(0), 0},
	}
	for _, tt := range tests {
		if got := tt.mtu.Bytes(); got != tt.want {
			t.Errorf("MTU(%d).Bytes() = %d, want %d", tt.mtu, got, tt.want)
		}
	}
}

func TestOpError(t *testing.T) {
	err := error(&verbs.OpError{Op: "ibv_open_device", Err: syscall.EINVAL})
	if want := "verbs: ibv_open_device: " + syscall.EINVAL.Error(); err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, syscall.EINVAL) {
		t.Errorf("errors.Is(err, syscall.EINVAL) = false, want true")
	}
	var opErr *verbs.OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_open_device" {
		t.Errorf("errors.As(err, *OpError) failed or Op mismatch: %v", err)
	}
}

func TestWCError(t *testing.T) {
	tests := []struct {
		wc   verbs.WC
		want string // "" means Err() must be nil
	}{
		{verbs.WC{Status: verbs.WCSuccess}, ""},
		{verbs.WC{Status: verbs.WCLocProtErr, VendorErr: 0x66}, "verbs: work completion: local protection error (vendor error 0x66)"},
		{verbs.WC{Status: verbs.WCWRFlushErr}, "verbs: work completion: Work Request Flushed Error (vendor error 0x0)"},
		{verbs.WC{Status: verbs.WCStatus(30), VendorErr: 1}, "verbs: work completion: WCStatus(30) (vendor error 0x1)"},
	}
	for _, tt := range tests {
		t.Run(tt.wc.Status.String(), func(t *testing.T) {
			err := tt.wc.Err()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Err() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Err() = %v, want %q", err, tt.want)
			}
			var wcErr *verbs.WCError
			if !errors.As(err, &wcErr) || wcErr.Status != tt.wc.Status || wcErr.VendorErr != tt.wc.VendorErr {
				t.Errorf("errors.As(err, *WCError) failed or fields mismatch: %#v", err)
			}
		})
	}
}

func TestWCHasImm(t *testing.T) {
	if (verbs.WC{Flags: verbs.WCGRH}).HasImm() {
		t.Error("HasImm() = true without WCWithImm")
	}
	if !(verbs.WC{Flags: verbs.WCWithImm, Imm: 7}).HasImm() {
		t.Error("HasImm() = false with WCWithImm")
	}
}
