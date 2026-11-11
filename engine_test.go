package rdma

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Config.Engine selects the engine behind the platform entry: the native
// engine for the zero value and EngineNative, the librdmacm engine for
// EngineLibrdmacm. A value that names no engine fails Dial and Listen with an
// error about the Config, which names it, before any engine is asked.
func TestConfigEngineSelectsTheEngine(t *testing.T) {
	for _, tt := range []struct {
		name   string
		engine Engine
		want   engine // nil when refused
	}{
		{"zero value", "", nativeEngine{}},
		{"EngineNative", EngineNative, nativeEngine{}},
		{"EngineLibrdmacm", EngineLibrdmacm, librdmacmEngine{}},
		{"unknown", "rsocket", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Engine: tt.engine}
			if tt.want != nil {
				if err := c.validate(); err != nil {
					t.Fatalf("Config{Engine: %q}.validate() = %v, want nil", tt.engine, err)
				}
				if got := platformEngine(c); reflect.TypeOf(got) != reflect.TypeOf(tt.want) {
					t.Errorf("platformEngine(Config{Engine: %q}) is a %T, want a %T", tt.engine, got, tt.want)
				}
				return
			}
			d := Dialer{Config: c, eng: untouchedEngine{t}}
			if _, err := d.Dial("rdma", "10.99.0.1:1"); !isOpError(err, "dial", errConfig) || !strings.Contains(err.Error(), `"rsocket"`) {
				t.Errorf("Dial() with Config{Engine: %q} error = %v, want a dial *net.OpError matching errConfig that names the value", tt.engine, err)
			}
			lc := ListenConfig{Config: c, eng: untouchedEngine{t}}
			if _, err := lc.Listen(t.Context(), "rdma", "10.99.0.1:0"); !isOpError(err, "listen", errConfig) || !strings.Contains(err.Error(), `"rsocket"`) {
				t.Errorf("Listen() with Config{Engine: %q} error = %v, want a listen *net.OpError matching errConfig that names the value", tt.engine, err)
			}
		})
	}
}

// untouchedEngine fails the test when it is asked to dial or listen.
type untouchedEngine struct{ t *testing.T }

func (e untouchedEngine) dial(context.Context, *Addr, *Addr, Config) (stream, error) {
	e.t.Error("the engine was asked to dial")
	return nil, errors.ErrUnsupported
}

func (e untouchedEngine) listen(*Addr, int, Config) (acceptor, error) {
	e.t.Error("the engine was asked to listen")
	return nil, errors.ErrUnsupported
}
