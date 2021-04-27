package main

import (
	"fmt"
	"net"
)

func main() {
	x := net.ParseIP("abc").To4()
	fmt.Println(x)

	s, err := IPType("10.244.3.4")
	if err != nil {
		panic(err)
	}
	fmt.Println(s)
}

func IPType(s string) (string, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("%s is not a valid IP", s)
	}
	ip = ip.To4()
	if ip == nil {
		return "IPv6", nil
	}
	return "IPv4", nil
}
