package cm

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
