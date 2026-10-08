package main

import "testing"

func TestParseTokens(t *testing.T) {
	m, err := parseTokens(" openwrt:aaaaaaaaaaaaaaaa , acme:bbbbbbbbbbbbbbbb,")
	if err != nil || m["aaaaaaaaaaaaaaaa"] != "openwrt" || m["bbbbbbbbbbbbbbbb"] != "acme" || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	if m, err := parseTokens(""); err != nil || len(m) != 0 {
		t.Fatalf("空串应得空表（PUSHME_TOKENS 必填由 run 检查）：%v %v", m, err)
	}
	for _, bad := range []string{
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

func TestParseReceivers(t *testing.T) {
	callers := map[string]string{"aaaaaaaaaaaaaaaa": "openwrt"}
	m, err := parseReceivers("phone:cccccccccccccccc", callers)
	if err != nil || m["cccccccccccccccc"] != "phone" {
		t.Fatalf("%v %v", m, err)
	}
	if m, err := parseReceivers("", callers); err != nil || len(m) != 0 {
		t.Fatalf("留空就是不开收件箱：%v %v", m, err)
	}
	if _, err := parseReceivers("phone:aaaaaaaaaaaaaaaa", callers); err == nil {
		t.Fatal("接收方与调用方共用 token 应报错")
	}
}
