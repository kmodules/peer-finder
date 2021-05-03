/*
Copyright AppsCode Inc. and Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
