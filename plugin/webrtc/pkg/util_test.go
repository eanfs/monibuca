package webrtc

import (
	"reflect"
	"testing"

	"github.com/mcuadros/go-defaults"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/twcc"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// chainInterceptorTypes 取出 Build 返回的拦截器链中各个拦截器的动态类型。
// pion/interceptor v0.1.51 的 Registry.Build 返回 *interceptor.Chain，
// 且不导出内部切片，因此通过反射读取；内部布局变化时测试会直接报错。
func chainInterceptorTypes(t *testing.T, i interceptor.Interceptor) []reflect.Type {
	t.Helper()
	chain, ok := i.(*interceptor.Chain)
	if !ok {
		t.Fatalf("expected *interceptor.Chain, got %T", i)
	}
	field := reflect.ValueOf(chain).Elem().FieldByName("interceptors")
	if !field.IsValid() || field.Kind() != reflect.Slice {
		t.Fatal("interceptor.Chain internal layout changed; update chainInterceptorTypes")
	}
	types := make([]reflect.Type, field.Len())
	for n := range types {
		v := field.Index(n)
		if v.Kind() == reflect.Interface {
			if v.IsNil() {
				t.Fatalf("nil interceptor at index %d", n)
			}
			v = v.Elem() // 取接口内的动态类型
		}
		types[n] = v.Type()
	}
	return types
}

func buildCCChainTypes(t *testing.T, m *pionwebrtc.MediaEngine) (withCC, withoutCC []reflect.Type) {
	t.Helper()
	withReg, err := createInterceptorRegistry(m, CCConfig{Enable: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutReg, err := createInterceptorRegistry(m, CCConfig{Enable: false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := withReg.Build("test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := withoutReg.Build("test")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	return chainInterceptorTypes(t, a), chainInterceptorTypes(t, b)
}

func TestCreateInterceptorRegistryCCEnabled(t *testing.T) {
	m := &pionwebrtc.MediaEngine{}
	registerLegacyCodecs(m)

	ai, bi := buildCCChainTypes(t, m)
	// cc 启用时链上多两个拦截器：cc 本体 + 写 transport-cc 扩展的 header extension
	if len(ai) != len(bi)+2 {
		t.Fatalf("cc enabled should add exactly two interceptors (cc + twcc header extension): got %d vs %d", len(ai), len(bi))
	}
}

func TestCreateInterceptorRegistryCCDefaults(t *testing.T) {
	m := &pionwebrtc.MediaEngine{}
	registerLegacyCodecs(m)
	// 启用了也不应因为 0 值 panic
	if _, err := createInterceptorRegistry(m, CCConfig{Enable: true, InitialBitrate: 0, MinBitrate: 0, MaxBitrate: 0}, nil); err != nil {
		t.Fatal(err)
	}

	cfg := CCConfig{Enable: true}.withDefaults()
	if cfg.InitialBitrate != 2000000 || cfg.MinBitrate != 300000 || cfg.MaxBitrate != 20000000 {
		t.Fatalf("unexpected normalized bitrates: %+v", cfg)
	}
}

func TestCreateInterceptorRegistryCCFirst(t *testing.T) {
	m := &pionwebrtc.MediaEngine{}
	registerLegacyCodecs(m)

	ai, bi := buildCCChainTypes(t, m)
	// cc 启用时链上多两个拦截器：cc 本体 + twcc header extension
	if len(ai) != len(bi)+2 {
		t.Fatalf("cc enabled should add exactly two interceptors (cc + twcc header extension): got %d vs %d", len(ai), len(bi))
	}

	ccType := reflect.TypeOf((*cc.Interceptor)(nil))
	twccType := reflect.TypeOf((*twcc.HeaderExtensionInterceptor)(nil))
	if got := ai[0]; got != ccType {
		t.Fatalf("cc interceptor must be first in Build order, got %v", got)
	}
	if got := ai[1]; got != twccType {
		t.Fatalf("twcc header extension interceptor must be second (after cc, before defaults), got %v", got)
	}
	for n, wp := range ai[2:] {
		if bp := bi[n]; wp != bp {
			t.Fatalf("interceptor %d mismatch: with cc %v, without cc %v", n, wp, bp)
		}
	}
	for n, it := range bi {
		if it == ccType {
			t.Fatalf("cc interceptor registered at index %d although cc.enable=false", n)
		}
		if it == twccType {
			t.Fatalf("twcc header extension interceptor registered at index %d although cc.enable=false", n)
		}
	}
}

func TestCreateInterceptorRegistryCCEstimatorCallback(t *testing.T) {
	tests := []struct {
		name        string
		config      CCConfig
		wantBitrate int
	}{
		{
			name:        "default config uses 2Mbps initial bitrate",
			config:      CCConfig{Enable: true},
			wantBitrate: 2_000_000,
		},
		{
			name:        "custom initial bitrate reaches gcc",
			config:      CCConfig{Enable: true, InitialBitrate: 4_000_000, MinBitrate: 1_000_000, MaxBitrate: 8_000_000},
			wantBitrate: 4_000_000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &pionwebrtc.MediaEngine{}
			registerLegacyCodecs(m)

			var got cc.BandwidthEstimator
			registry, err := createInterceptorRegistry(m, tt.config, func(e cc.BandwidthEstimator) {
				got = e
			})
			if err != nil {
				t.Fatal(err)
			}
			built, err := registry.Build("test-pc")
			if err != nil {
				t.Fatal(err)
			}
			defer built.Close()

			if got == nil {
				t.Fatal("onEstimator callback was not invoked when the peer connection chain was built")
			}
			// 必须精确等于配置的初始码率：pion gcc 的默认 latestBitrate 是 10000，
			// 只断言 > 0 无法区分“配置生效”与“配置被丢弃、退回默认值”。
			if bitrate := got.GetTargetBitrate(); bitrate != tt.wantBitrate {
				t.Fatalf("estimator target bitrate = %d, want %d", bitrate, tt.wantBitrate)
			}
		})
	}
}

// TestCCConfigStructTags 钉住 CCConfig 的 default struct tag，
// 防止默认值只存在于常量/默认 yaml 中而配置系统解析出零值。
func TestCCConfigStructTags(t *testing.T) {
	var cfg CCConfig
	defaults.SetDefaults(&cfg)
	if !cfg.Enable {
		t.Error("default enable = false, want true")
	}
	if cfg.InitialBitrate != 2_000_000 {
		t.Errorf("default initialbitrate = %d, want 2000000", cfg.InitialBitrate)
	}
	if cfg.MinBitrate != 300_000 {
		t.Errorf("default minbitrate = %d, want 300000", cfg.MinBitrate)
	}
	if cfg.MaxBitrate != 20_000_000 {
		t.Errorf("default maxbitrate = %d, want 20000000", cfg.MaxBitrate)
	}
}
