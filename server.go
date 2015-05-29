// Package benchserve provides a simple benchmark server.
//
// It is designed to allow an external program to drive
// the benchmarks in a compiled test binary.
//
// The API and protocol is still under development and may change.
//
// To enable benchserve with a package, add this somewhere to your
// package's tests:
//
// 	import "github.com/josharian/benchserve"
//
// 	func TestMain(m *testing.M) {
// 		benchserve.Main(m)
// 	}
//
// Your existing tests and benchmarks will operate unchanged.
// To use benchserve, compile the tests with 'go test -c',
// and then execute with the -test.benchserve flag,
// e.g. './foo.test -test.benchserve'.
// This will bypass all tests and benchmarks and ignore all other
// flags, including the usual benchmarking and profiling flags,
// and instead start the benchmark server.
//
// The benchmark server uses JSON-RPC.
// By default, it listens on :52525. Use the -test.benchserve.addr
// flag to set a different host:port.
// The server only serves a single request at a time.
// Serving requests concurrency could skew benchmark results.
//
// Benchserve relies on unexported details of the testing package,
// which may change at any time. A request to officially support
// this functionality is https://golang.org/issue/10930.
package benchserve

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

var (
	benchServe     = flag.Bool("test.benchserve", false, "run a JSON-RPC benchmark server")
	benchServeAddr = flag.String("test.benchserve.addr", ":52525", "`host:port` for the JSON-RPC benchmark server")
)

// Main runs a test binary.
// To incorporate benchserve into your package,
// add this TestMain function:
//
// 	func TestMain(m *testing.M) {
// 		benchserve.Main(m)
// 	}
//
// If your package already has a TestMain, use Serve.
func Main(m *testing.M) {
	flag.Parse()
	Serve(m)
	os.Exit(m.Run())
}

// Serve starts a new benchmark server using the benchmarks contained in m
// if the test.benchserve flag is set. Otherwise, Serve is a no-op.
//
// Serve should only be used in packages that already have a custom TestMain function.
// Most packages should use Main instead.
//
// To use Serve, call it from TestMain after calling flag.Parse, after any required
// benchmarking setup has completed, but before any tests or benchmarks have been run.
// For example:
//
// 	func TestMain(m *testing.M) {
// 		flag.Parse()
//  	// do any setup that is necessary for benchmarking
// 		benchserve.Serve() // if flag is set, does not return; if flag is not set, no-op
// 		// run tests, etc.
// 	}
func Serve(m *testing.M) {
	if !*benchServe {
		return
	}
	newServer(m).serve()
	os.Exit(0)
}

// Server is a benchmark server.
// It handles JSON-RPC requests.
type Server struct {
	m   map[string]testing.InternalBenchmark
	opt Options
}

// Options control benchmarking behavior.
type Options struct {
	Benchmem bool // equivalent to -test.benchmem
}

// Run requests a single benchmark run.
type Run struct {
	Name  string // name of the benchmark to run
	Procs int    // GOMAXPROCS value, equivalent to -test.cpu
	N     int    // number of iterations to run, equivalent to b.N
}

// Result is the result of a single benchmark run.
type Result struct {
	testing.BenchmarkResult

	// ReportAllocs reports whether allocations should be reported for this run.
	// This might be set as a result of the current Options
	// or because the benchmark called b.ReportAllocs.
	ReportAllocs bool

	// failed reports whether the benchmark run failed.
	failed bool
}

func newServer(m *testing.M) *Server {
	v := reflect.ValueOf(m).Elem().FieldByName("benchmarks")
	benchmarks := *(*[]testing.InternalBenchmark)(unsafe.Pointer(v.UnsafeAddr())) // :(((

	s := Server{m: make(map[string]testing.InternalBenchmark)}
	for _, b := range benchmarks {
		if _, ok := s.m[b.Name]; ok {
			// It is possible to define a benchmark with the same name
			// twice in a single test binary, by defining it once
			// in a regular test package and once in an external test package.
			// Don't do that.
			log.Fatalf("found two benchmarks named %s", b.Name)
		}
		s.m[b.Name] = b
	}

	return &s
}

// Serve starts the server. It blocks.
func (s *Server) serve() {
	rpc.Register(s)

	l, err := net.Listen("tcp", *benchServeAddr)
	if err != nil {
		log.Fatalf("listen %v: %v", *benchServeAddr, err)
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatalf("accept: %v", err)
		}
		jsonrpc.ServeConn(conn)
		conn.Close()
	}
}

// List returns an unordered list of the available benchmark names.
func (s *Server) List(args struct{}, names *[]string) error {
	for _, b := range s.m {
		*names = append(*names, b.Name)
	}
	return nil
}

// Kill stops the benchmark server and its process.
func (s *Server) Kill(args struct{}, reply *struct{}) error {
	os.Exit(0)
	return nil
}

// Set sets the server's Options.
func (s *Server) Set(args Options, reply *struct{}) error {
	s.opt = args
	return nil
}

// Run runs a single benchmark.
func (s *Server) Run(args Run, reply *Result) error {
	b, ok := s.m[args.Name]
	if !ok {
		return fmt.Errorf("%s not found", args.Name)
	}

	runtime.GOMAXPROCS(args.Procs)
	*reply = runBenchmark(b, args.N)

	if reply.failed {
		return fmt.Errorf("%s failed", args.Name)
	}

	if p := runtime.GOMAXPROCS(-1); p != args.Procs {
		return fmt.Errorf("%s left GOMAXPROCS set to %d\n", b.Name, p)
	}

	return nil
}

// runBenchmark runs b for the specified number of iterations.
func runBenchmark(b testing.InternalBenchmark, n int) Result {
	var wg sync.WaitGroup
	wg.Add(1)
	tb := testing.B{N: n}
	tb.SetParallelism(1)

	go func() {
		defer wg.Done()
		// Try to get a comparable environment for each run
		// by clearing garbage from previous runs.
		runtime.GC()
		tb.ResetTimer()
		tb.StartTimer()
		b.F(&tb)
		tb.StopTimer()
	}()
	wg.Wait()

	v := reflect.ValueOf(tb)
	var r Result
	r.N = n
	r.T = time.Duration(v.FieldByName("duration").Int())
	r.Bytes = v.FieldByName("bytes").Int()
	r.MemAllocs = v.FieldByName("netAllocs").Uint()
	r.MemBytes = v.FieldByName("netBytes").Uint()
	r.ReportAllocs = v.FieldByName("showAllocResult").Bool()
	r.failed = v.FieldByName("failed").Bool()
	return r
}
