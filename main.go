package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"

	"gopkg.in/yaml.v2"
)

type Config struct {
	IDs  map[int]string `yaml:"ids"`
	Heap map[int]struct {
		Left  int `yaml:"left"`
		Right int `yaml:"right"`
	} `yaml:"heap"`
}

var (
	confPath   = flag.String("config", "", "Path to the config file")
	id         = flag.Int("id", -1, "ID of this node")
	memprofile = flag.String("memprofile", "", "write memory profile to this file")
	cpuprofile = flag.String("cpuprofile", "", "write cpu profile to this file")
)

var be = binary.BigEndian

func main() {
	flag.Parse()

	if confPath == nil || *confPath == "" {
		fmt.Fprint(os.Stderr, "'config' flag is required\n")
		flag.Usage()
		os.Exit(2)
	}

	if id == nil || *id == -1 {
		fmt.Fprint(os.Stderr, "'id' flag is required\n")
		flag.Usage()
		os.Exit(2)
	}

	configFile, err := os.Open(*confPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error while opening config file: %s\n", err.Error())
		os.Exit(2)
	}

	var config Config
	err = yaml.NewDecoder(configFile).Decode(&config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error while reading config file: %s\n", err.Error())
		os.Exit(2)
	}

	addr := config.IDs[*id]
	if addr == "" {
		fmt.Fprintf(os.Stderr, "ID of node not in config\n")
		os.Exit(2)
	}

	listener, err := net.ListenPacket("udp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error while starting listener: %s\n", err.Error())
		os.Exit(2)
	}

	manager := RateLimitManager{
		IncomingListener: listener,
	}

	for parent, children := range config.Heap {
		if parent == *id {
			addr, err := net.ResolveUDPAddr("udp", config.IDs[children.Left])
			if err == nil {
				manager.LeftAddr = addr
			}

			addr, err = net.ResolveUDPAddr("udp", config.IDs[children.Right])
			if err == nil {
				manager.RightAddr = addr
			}
		}

		if children.Left == *id || children.Right == *id {
			addr, err := net.ResolveUDPAddr("udp", config.IDs[parent])
			if err == nil {
				manager.ParentAddr = addr
			}
		}
	}

	manager.Init(1000)
	perSrc := &RateLimiter{
		SyncInterval: int64(100 * time.Millisecond),
		LimitStrategy: &BucketLimiter{
			BucketSize:    50,
			ResetInterval: int64(60 * time.Second),
		},
	}
	manager.Add("per-src", perSrc)
	// perDst := &RateLimiter{
	// 	SyncInterval: int64(100 * time.Millisecond),
	// 	LimitStrategy: &BucketLimiter{
	// 		BucketSize:    50,
	// 		ResetInterval: int64(10 * time.Second),
	// 	},
	// }
	// manager.Add("per-dst", perDst)

	// go func() {
	// 	for {
	// 		for i := 0; i < 1000; i++ {
	// 			perSrc.CheckAndIncrement(strconv.Itoa(i))
	// 		}
	// 		time.Sleep(100 * time.Millisecond)
	// 	}
	// }()

	go sim(-1, 2, perSrc, "abc")

	// +10 req/s on node 4
	go sim(4, 10, perSrc, "abc")

	fmt.Printf("%d: running\n", *id)

	sigChan := make(chan os.Signal, 3)
	signal.Notify(sigChan, os.Interrupt)

	go func() {
		var cpuF *os.File
		if *cpuprofile != "" {
			cpuF, err = os.Create(*cpuprofile)
			if err != nil {
				log.Fatal(err)
			}
			pprof.StartCPUProfile(cpuF)
		}

		<-sigChan
		if *memprofile != "" {
			f, err := os.Create(*memprofile)
			if err != nil {
				log.Fatal(err)
			}
			pprof.WriteHeapProfile(f)
			f.Close()
		}

		if cpuF != nil {
			pprof.StopCPUProfile()
			cpuF.Close()
		}

		fmt.Println("Exiting")
		os.Exit(0)
	}()

	manager.Start()
}

func sim(symID int, rate int, r *RateLimiter, key string) {
	if symID != -1 && *id == symID {
		return
	}
	for {
		r.CheckAndIncrement(key)
		time.Sleep(time.Second / time.Duration(rate))
	}
}

type SyncRequest struct {
	limiterName string
	key         string
	values      *Values
}

type RateLimitManager struct {
	ParentAddr *net.UDPAddr
	LeftAddr   *net.UDPAddr
	RightAddr  *net.UDPAddr

	IncomingListener net.PacketConn

	limiters map[string]*RateLimiter
	syncChan chan SyncRequest
}

func (rlm *RateLimitManager) Init(syncChanCapacity int) {
	if rlm.limiters == nil {
		rlm.limiters = make(map[string]*RateLimiter)
	}

	if rlm.syncChan == nil {
		rlm.syncChan = make(chan SyncRequest, syncChanCapacity)
	}
}

func (rlm *RateLimitManager) Start() {
	// TODO give ctx or cancel chan's to routines
	go rlm.sender()
	rlm.receiver()
}

func (rlm *RateLimitManager) CheckAndIncrement(limiterName, key string) bool {
	limiter := rlm.limiters[limiterName]
	if limiter == nil {
		return false
	}

	return limiter.CheckAndIncrement(key)
}

// We will at max send packets with this amount of bytes. Min MTU is 1500 so 1400 is a safe, leaves some room for
// headers and encapsulation.
// TODO make configurable, since increased MTU can be configured on the local network
const packetLimit = 1400

func (rlm *RateLimitManager) sender() {
	var (
		parentPacket bytes.Buffer
		leftPacket   bytes.Buffer
		rightPacket  bytes.Buffer
	)

	i := byte(0)

	reset := func() {
		// Reset the packet buffer
		parentPacket.Reset()
		leftPacket.Reset()
		rightPacket.Reset()

		// The first byte contains the amount of records sent over.
		// We don't know how many we will send yet, so just write 0, we will overwrite it before sending
		parentPacket.WriteByte(0)
		leftPacket.WriteByte(0)
		rightPacket.WriteByte(0)

		i = 0
	}

	// Allocate 8 bytes once and reuse them for all uint64 to bytes conversion to avoid allocations
	uint64Buf := make([]byte, 8)
	// Allocate 8 bytes once and reuse them for all uint64 to bytes conversion to avoid allocations
	uint16Buf := make([]byte, 2)

	write := func(req SyncRequest) {
		// Write the limiter length followed by the string
		limiter := []byte(req.limiterName)
		be.PutUint16(uint16Buf, uint16(len(limiter)))
		parentPacket.Write(uint16Buf)
		parentPacket.Write(limiter)
		leftPacket.Write(uint16Buf)
		leftPacket.Write(limiter)
		rightPacket.Write(uint16Buf)
		rightPacket.Write(limiter)

		// Write the key length followed by the string
		key := []byte(req.key)
		be.PutUint16(uint16Buf, uint16(len(key)))
		parentPacket.Write(uint16Buf)
		parentPacket.Write(key)
		leftPacket.Write(uint16Buf)
		leftPacket.Write(key)
		rightPacket.Write(uint16Buf)
		rightPacket.Write(key)

		// Write the values to increment
		be.PutUint64(uint64Buf, atomic.SwapUint64(&req.values.toParent, 0))
		parentPacket.Write(uint64Buf)
		be.PutUint64(uint64Buf, atomic.SwapUint64(&req.values.toLeft, 0))
		leftPacket.Write(uint64Buf)
		be.PutUint64(uint64Buf, atomic.SwapUint64(&req.values.toRight, 0))
		rightPacket.Write(uint64Buf)

		// Reset the syncRequested flag and sync time since we have added the values to the packet to be sent
		atomic.StoreUint32(&req.values.syncRequested, 0)
		atomic.StoreInt64(&req.values.sync, runtimeNano())

		// Increase the packet count
		i++
	}

	send := func() {
		// Write the amount of counters in the packet as the first byte
		parentPacket.Bytes()[0] = i
		leftPacket.Bytes()[0] = i
		rightPacket.Bytes()[0] = i

		if rlm.ParentAddr != nil {
			rlm.IncomingListener.WriteTo(parentPacket.Bytes(), rlm.ParentAddr)
		}

		if rlm.LeftAddr != nil {
			rlm.IncomingListener.WriteTo(leftPacket.Bytes(), rlm.LeftAddr)
		}

		if rlm.RightAddr != nil {
			rlm.IncomingListener.WriteTo(rightPacket.Bytes(), rlm.RightAddr)
		}
	}

	reset()
outer:
	for req := range rlm.syncChan {
		write(req)

		// Read from the channel until it is empty or we have buffered 100 requests
		stop := false
		for !stop {
			select {
			case req = <-rlm.syncChan:
				// Check if the new entry plus the existing packet buffer will exceed the packet limit.
				// We use parentPacket, assuming all packets are the same length
				if len(req.limiterName)+len(req.key)+8+parentPacket.Len() > packetLimit {
					// Edge case: If for some reason the limiterName plus key and 8 bytes itself is larger than the
					// packetLimit. Could happen if the key becomes very large for some reason.
					// Handle it by discarding the request
					if len(req.limiterName)+len(req.key)+8 > packetLimit {
						goto outer
					}

					// Send the current packet buffers
					send()
					// Reset the buffers
					reset()
					// Write the leftover request to the buffers
					write(req)
					// Go the the outer loop
					goto outer
				}

				write(req)
			default:
				stop = true
			}
		}

		send()
		reset()
	}
}

func (rlm *RateLimitManager) receiver() {
	buf := make([]byte, 1<<16)
	for {
		n, addr, err := rlm.IncomingListener.ReadFrom(buf)
		if err != nil {
			fmt.Printf("%d: err: %s\n", *id, err.Error())
			continue
		}

		const (
			fromParent = 1
			fromLeft   = 2
			fromRight  = 3
		)

		from := 0
		switch addr.String() {
		case rlm.ParentAddr.String():
			from = fromParent
		case rlm.LeftAddr.String():
			from = fromLeft
		case rlm.RightAddr.String():
			from = fromRight
		default:
			continue
		}

		// fmt.Printf("%s -> %d: %d\n", addr.String(), *id, n)

		msg := buf[:n]
		off := 1
		for i := byte(0); i < msg[0]; i++ {
			limitterLen := be.Uint16(msg[off : off+2])
			off += 2

			limiterName := string(msg[off : off+int(limitterLen)])
			off += int(limitterLen)

			keyLen := be.Uint16(msg[off : off+2])
			off += 2

			key := string(msg[off : off+int(keyLen)])
			off += int(keyLen)

			value := be.Uint64(msg[off : off+8])
			off += 8

			r := rlm.limiters[limiterName]
			values := r.vm.GetOrCreate(key)

			// Always increment the local total
			localTotal := atomic.AddUint64(&values.localTotal, value)
			// atomic.AddUint64(&values.localTotal, value)

			// And update all the pending values, except to the nodes from which we received the update
			// of we don't we will get a loop
			switch from {
			case fromParent:
				atomic.AddUint64(&values.toLeft, value)
				atomic.AddUint64(&values.toRight, value)
			case fromLeft:
				atomic.AddUint64(&values.toParent, value)
				atomic.AddUint64(&values.toRight, value)
			case fromRight:
				atomic.AddUint64(&values.toParent, value)
				atomic.AddUint64(&values.toLeft, value)
			}

			// If a sync to other nodes is overdue
			if runtimeNano()-atomic.LoadInt64(&values.sync) >= r.SyncInterval {
				// Compare syncRequested and 0, if they are equal change syncRequested to 1.
				// If syncRequested was changed, the function returns true.
				if atomic.CompareAndSwapUint32(&values.syncRequested, 0, 1) {
					select {
					case r.syncChan <- SyncRequest{
						limiterName: limiterName,
						key:         key,
						values:      values,
					}:
						// Empty on purpose, just to avoid blocking if syncChan is full
					default:
						// If we were unable to submit to the syncChan, reset the syncRequested flag so submitting can happen
						// again next try
						atomic.StoreUint32(&values.syncRequested, 0)
					}
				}
			}

			d := time.Duration(runtimeNano() - startNano)
			fmt.Printf(
				"%3d.%3d %2d: %s => %s/%s = %4d Δ=%d\n",
				int(d.Seconds()),
				d.Milliseconds(),
				*id,
				addr.String(),
				limiterName,
				key,
				localTotal,
				value,
			)
		}
	}
}

func (rlm *RateLimitManager) Add(name string, limiter *RateLimiter) {
	if limiter.vm.kv == nil {
		limiter.vm.kv = make(map[string]*Values)
	}

	limiter.name = name
	rlm.limiters[name] = limiter
	limiter.syncChan = rlm.syncChan
}

type RateLimiter struct {
	name string
	// All values for this rate limiter
	vm ValuesMap
	// The channel used to request synchronization updates to other nodes
	syncChan chan SyncRequest

	// Interval in nanoseconds, how often to send updates to other nodes
	SyncInterval int64
	// The strategy used to limit the requests
	LimitStrategy LimitStrategy
}

func (r *RateLimiter) CheckAndIncrement(key string) bool {
	return r.LimitStrategy.CheckAndIncrement(r.name, key, r)
}

type ValuesMap struct {
	mu sync.RWMutex
	kv map[string]*Values
}

func (vm *ValuesMap) GetOrCreate(key string) *Values {
	vm.mu.RLock()
	values := vm.kv[key]
	vm.mu.RUnlock()

	if values == nil {
		vm.mu.Lock()
		values = &Values{
			sync: runtimeNano(),
			t:    runtimeNano(),
		}
		vm.kv[key] = values
		vm.mu.Unlock()
	}

	return values
}

func (vm *ValuesMap) Delete(key string) {
	vm.mu.RLock()
	defer vm.mu.RUnlock()

	delete(vm.kv, key)
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

type LimitStrategy interface {
	CheckAndIncrement(limiterName, key string, r *RateLimiter) bool
}

type BucketLimiter struct {
	// The size of the bucket
	BucketSize uint64
	// Interval in nanoseconds, after which the bucket will be reset
	ResetInterval int64
}

// runtimeNano returns the current value of the runtime clock in nanoseconds.
// We use this instead of the time package because of 2 reasons, the first is that time.Time is a struct containing both
// monotonic time and wall time. Since we only need monotonic time this saves us data.
// The second reason is that we can use atomic functions to read and write the int64 nanotime values, which we can't do
// with the private fields of time.Time, so this is way better from a concurrency perspective.
//go:linkname runtimeNano runtime.nanotime
func runtimeNano() int64

// Monotonic times are reported as offsets from startNano.
// We initialize startNano to runtimeNano() - 1 so that on systems where
// monotonic time resolution is fairly low (e.g. Windows 2008
// which appears to have a default resolution of 15ms),
// we avoid ever reporting a monotonic time of 0.
// (Callers may want to use 0 as "time not set".)
var startNano int64 = runtimeNano() - 1

// CheckAndIncrement increments counters and returns true if the rate limit is exceeded.
func (bl BucketLimiter) CheckAndIncrement(limiterName, key string, r *RateLimiter) bool {
	values := r.vm.GetOrCreate(key)

	now := runtimeNano()
	// If it has been longer than ResetInterval since the bucket was reset
	if (now - atomic.LoadInt64(&values.t)) > bl.ResetInterval {
		// Set the last reset time to now
		atomic.StoreInt64(&values.t, now)
		// Set the local counter to 1
		atomic.StoreUint64(&values.localTotal, 1)
		// Don't block since we just reset the counter
		return false
	}

	// Increment all pending counters
	atomic.AddUint64(&values.toParent, 1)
	atomic.AddUint64(&values.toLeft, 1)
	atomic.AddUint64(&values.toRight, 1)

	// If a sync to other nodes is overdue
	if now-atomic.LoadInt64(&values.sync) >= r.SyncInterval {
		// Compare syncRequested and 0, if they are equal change syncRequested to 1.
		// If syncRequested was changed, the function returns true.
		if atomic.CompareAndSwapUint32(&values.syncRequested, 0, 1) {
			select {
			case r.syncChan <- SyncRequest{
				limiterName: limiterName,
				key:         key,
				values:      values,
			}:
				// Empty on purpose, just to avoid blocking if syncChan is full
			default:
				// If we were unable to submit to the syncChan, reset the syncRequested flag so submitting can happen
				// again next try
				atomic.StoreUint32(&values.syncRequested, 0)
			}
		}
	}

	// Increment the local total as well, check the result after incrementing to see if the bucket size has been
	// exceeded.
	return atomic.AddUint64(&values.localTotal, 1) >= bl.BucketSize
}
