package main

import (
	"reflect"
	"testing"

	"github.com/fortaegis/plasma/internal/plasma"
)

func TestParseDropRule(t *testing.T) {
	got, err := parseDropRule("proto=udp,src=10.77.0.1,sport=4242,u8@31=0x42,u8@34=5/0x0f")
	if err != nil {
		t.Fatal(err)
	}
	want := plasma.DropRule{Protocol: 17, Src: "10.77.0.1", Sport: 4242,
		Bytes: []plasma.ByteMatch{{Offset: 31, Value: 0x42}, {Offset: 34, Value: 5, Mask: 0x0f}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	for _, bad := range []string{"proto", "colour=red", "sport=x", "u8@x=1"} {
		if _, err := parseDropRule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseTunnel(t *testing.T) {
	cases := map[string]tunnelSpec{
		"fabric:9200":              {bind: "127.0.0.1:9200", vm: "fabric", port: 9200},
		"3001:fabric:3000":         {bind: "127.0.0.1:3001", vm: "fabric", port: 3000},
		"0.0.0.0:3000:fabric:3000": {bind: "0.0.0.0:3000", vm: "fabric", port: 3000},
	}
	for in, want := range cases {
		if got, err := parseTunnel(in); err != nil || got != want {
			t.Errorf("parseTunnel(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"fabric", "fabric:0", "x:fabric:80", "a:b:c:d:e"} {
		if _, err := parseTunnel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
