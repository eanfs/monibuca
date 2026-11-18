package webrtc

import (
	"reflect"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
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

func TestCreateInterceptorRegistryCCEnabled(t *testing.T) {
	m := &pionwebrtc.MediaEngine{}
	registerLegacyCodecs(m)

	withCC, err := createInterceptorRegistry(m, CCConfig{Enable: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutCC, err := createInterceptorRegistry(m, CCConfig{Enable: false}, nil)
	if err != nil {
		t.Fatal(err)
	}

	a, err := withCC.Build("test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := withoutCC.Build("test")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	ai := chainInterceptorTypes(t, a)
	bi := chainInterceptorTypes(t, b)
	if len(ai) != len(bi)+1 {
		t.Fatalf("cc enabled should add exactly one interceptor: got %d vs %d", len(ai), len(bi))
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

	withCC, err := createInterceptorRegistry(m, CCConfig{Enable: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutCC, err := createInterceptorRegistry(m, CCConfig{Enable: false}, nil)
	if err != nil {
		t.Fatal(err)
	}

	a, err := withCC.Build("test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := withoutCC.Build("test")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	ai := chainInterceptorTypes(t, a)
	bi := chainInterceptorTypes(t, b)
	if len(ai) != len(bi)+1 {
		t.Fatalf("cc enabled should add exactly one interceptor: got %d vs %d", len(ai), len(bi))
	}

	ccType := reflect.TypeOf((*cc.Interceptor)(nil))
	if got := ai[0]; got != ccType {
		t.Fatalf("cc interceptor must be first in Build order, got %v", got)
	}
	for n, wp := range ai[1:] {
		if bp := bi[n]; wp != bp {
			t.Fatalf("interceptor %d mismatch: with cc %v, without cc %v", n, wp, bp)
		}
	}
	for n, it := range bi {
		if it == ccType {
			t.Fatalf("cc interceptor registered at index %d although cc.enable=false", n)
		}
	}
}

func TestCreateInterceptorRegistryCCEstimatorCallback(t *testing.T) {
	m := &pionwebrtc.MediaEngine{}
	registerLegacyCodecs(m)

	var got cc.BandwidthEstimator
	registry, err := createInterceptorRegistry(m, CCConfig{Enable: true}, func(e cc.BandwidthEstimator) {
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
	if bitrate := got.GetTargetBitrate(); bitrate <= 0 {
		t.Fatalf("estimator target bitrate = %d, want > 0", bitrate)
	}
}
