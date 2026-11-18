package rdmacm

import "testing"

func TestConnParamDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   *ConnParam
		want ConnParam
	}{
		{"nil", nil, ConnParam{ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}},
		{"zero", &ConnParam{}, ConnParam{ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}},
		{"private data only", &ConnParam{PrivateData: []byte("hi")},
			ConnParam{PrivateData: []byte("hi"), ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}},
		{"explicit", &ConnParam{ResponderResources: 4, InitiatorDepth: 2, RetryCount: 3, RNRRetryCount: 5},
			ConnParam{ResponderResources: 4, InitiatorDepth: 2, RetryCount: 3, RNRRetryCount: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.withDefaults()
			if string(got.PrivateData) != string(tt.want.PrivateData) || got.ResponderResources != tt.want.ResponderResources ||
				got.InitiatorDepth != tt.want.InitiatorDepth || got.RetryCount != tt.want.RetryCount || got.RNRRetryCount != tt.want.RNRRetryCount {
				t.Errorf("withDefaults() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The options a Resolve or a Listen takes, which a Listener reads at every
// request, keep their values whatever the caller's variables hold afterwards.
func TestOptionsKeepTheValuesTaken(t *testing.T) {
	tos, afOnly, timeout := uint8(0x20), true, uint8(16)
	taken := []struct {
		of string
		o  options
	}{
		{"Dialer", Dialer{TOS: &tos, AFOnly: &afOnly, AckTimeout: &timeout}.options()},
		{"ListenConfig", ListenConfig{TOS: &tos, AFOnly: &afOnly, AckTimeout: &timeout}.options()},
	}
	tos, afOnly, timeout = 0xff, false, 200
	for _, tt := range taken {
		if *tt.o.tos != 0x20 || !*tt.o.afOnly || *tt.o.ackTimeout != 16 {
			t.Errorf("options of the %s once the variables changed: TOS %d, AFOnly %v, ACK timeout %d; want 32, true, 16",
				tt.of, *tt.o.tos, *tt.o.afOnly, *tt.o.ackTimeout)
		}
	}
}
