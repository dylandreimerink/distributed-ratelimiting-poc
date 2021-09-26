package main

import (
	"fmt"
	"math/rand"
	"runtime"
)

// This is a test to see the memory requirements for x amount of counters

const size = 10000000
const strlen = 16

func main() {
	m := make(map[string]*Values)

	p := 10
	for i := 0; i < size; i++ {
		m[randStr(strlen)] = &Values{}

		if i == p {
			p = p * 10
			runtime.GC()
			fmt.Println(i)
			PrintMemUsage()
			fmt.Println("-----")
		}
	}

	runtime.GC()
	fmt.Println(size)
	PrintMemUsage()
}

type Values struct {
	// The pending count yet to be sent to the parent
	toParent uint64
	// The pending count yet to be sent to the left node
	toLeft uint64
	// The pending count yet to be sent to the right node
	toRight uint64
	// Internal total counter
	localTotal uint64

	// The time in nanoseconds relative to the current runtimeNano value.
	// Depending on the rate limit strategy it indicates the last reset or decrement time.
	t int64
	// The time in nanoseconds relative to the current runtimeNano value when the last sync to other nodes for this
	// value occurred.
	sync int64
	// If 0 no sync has been requested, if 1 a sync has already been requested and is pending transmission.
	// A uint32 is used instread of a boolean since this is the smallest type that works with atomic
	syncRequested uint32
}

// PrintMemUsage outputs the current, total and OS memory being used. As well as the number
// of garage collection cycles completed.
func PrintMemUsage() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// For info on each, see: https://golang.org/pkg/runtime/#MemStats
	fmt.Printf("Alloc = %v MiB", bToMb(m.Alloc))
	fmt.Printf("\tTotalAlloc = %v MiB", bToMb(m.TotalAlloc))
	fmt.Printf("\tSys = %v MiB", bToMb(m.Sys))
	fmt.Printf("\tNumGC = %v\n", m.NumGC)
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}

const alphabet = "abcdefghijklmnopqrstuvwxyz"

func randStr(strlen int) string {
	str := make([]byte, strlen)
	for j := 0; j < strlen; j++ {
		str[j] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(str)
}
