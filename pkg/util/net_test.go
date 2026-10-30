package util

import "testing"

func TestRedactURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "rtsp with credentials",
			in:   "rtsp://admin:a1234567@192.168.1.16:554/Streaming/Channels/101",
			want: "rtsp://admin:xxxxx@192.168.1.16:554/Streaming/Channels/101",
		},
		{
			name: "http query kept",
			in:   "http://user:pass@example.com/live.flv?token=abc",
			want: "http://user:xxxxx@example.com/live.flv?token=abc",
		},
		{
			name: "no credentials",
			in:   "rtsp://192.168.1.16:554/cam",
			want: "rtsp://192.168.1.16:554/cam",
		},
		{
			name: "unparseable kept as is",
			in:   "http://[::1",
			want: "http://[::1",
		},
		{
			name: "unparseable with credentials masked",
			in:   "rtsp://admin:a1234567@[::1",
			want: "rtsp://xxxxx@[::1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RedactURL(c.in); got != c.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
