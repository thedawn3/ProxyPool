package main

import (
	"fmt"
	"github.com/thedawn3/ProxyPool_t/ProxyPool"
)

func main() {
	P := ProxyPool.GetProxy(60)
	fmt.Println(P)
}
