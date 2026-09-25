package main

import "testing"

func TestParseTokens(t *testing.T) {
	m, err := parseTokens(" openwrt:aaaaaaaaaaaaaaaa , acme:bbbbbbbbbbbbbbbb,")
	if err != nil || m["aaaaaaaaaaaaaaaa"] != "openwrt" || m["bbbbbbbbbbbbbbbb"] != "acme" || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{
		"",
		"openwrt",
		"openwrt:short",
		"a:aaaaaaaaaaaaaaaa,a:bbbbbbbbbbbbbbbb",
		"a:aaaaaaaaaaaaaaaa,b:aaaaaaaaaaaaaaaa",
		":aaaaaaaaaaaaaaaa",
	} {
		if _, err := parseTokens(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
}
