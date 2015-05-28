// Package benchserve provides a simple line-oriented benchmark server.
//
// It is designed to allow an external program to drive the benchmarks
// found in a compiled test binary.
//
// The protocol is still under development and may change.
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
// Your existing tests and benchmarks should operate unchanged.
// To use benchserve, compile the tests with 'go test -c',
// and then execute with the -test.benchserve flag,
// e.g. './foo.test -test.benchserve'.
// This will bypass all tests and benchmarks, and ignore all other
// flags, including the usual benchmarking and profiling flags,
// and instead start a benchmark server.
// The benchmark server accepts commands in stdin, prints output
// on stdout, and prints errors on stderr.
//
// It is designed to be invoked and driven by another program,
// but you can take a quick tour by hand. Here is a sample session:
//
// 	$ cd $GOROOT/src/encoding/json
// 	$ go test -c
// 	$ ./json.test -test.benchserve
// 	help
// 	commands: help, list, run, set, quit, exit
// 	list
// 	BenchmarkCodeEncoder
// 	BenchmarkCodeMarshal
// 	BenchmarkCodeDecoder
// 	BenchmarkCodeUnmarshal
// 	BenchmarkCodeUnmarshalReuse
// 	BenchmarkUnmarshalString
// 	BenchmarkUnmarshalFloat64
// 	BenchmarkUnmarshalInt64
// 	BenchmarkSkipValue
// 	BenchmarkEncoderEncode
//
// 	run BenchmarkCodeEncoder 100
// 	BenchmarkCodeEncoder	     100	  17719109 ns/op	 109.51 MB/s
// 	set benchmem true
// 	run BenchmarkCodeEncoder 100
// 	BenchmarkCodeEncoder	     100	  17974625 ns/op	 107.96 MB/s	   45953 B/op	       1 allocs/op
// 	run BenchmarkCodeEncoder-4 100
// 	BenchmarkCodeEncoder-4	     100	  18031952 ns/op	 107.61 MB/s	   45979 B/op	       1 allocs/op
// 	exit
// 	$
//
// The github.com/josharian/benchserve/benchcli package is a Go client
// for benchserve.
//
// The decision to use a simple, line-oriented server using pipes is intentional.
// This enables benchserve to rely only on the same set of packages that
// the testing package does, which means that it is usable with any package
// without introducing circular imports. Using (say) net/http or net/rpc or
// even just net would cause benchserve to be unsuitable for use with
// many packages in the standard library.
//
// Benchserve relies on unexported details of the testing package, which may
// change at any time. A request to officially support this functionality
// is https://golang.org/issue/10930.
package benchserve

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

var benchserve = flag.Bool("test.benchserve", false, "run an interactive benchmark server")

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
// Serve should only be used in packages that already have a custom TestMain function;
// most package should use Main instead.
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
	if !*benchserve {
		return
	}
	newServer(m).serve()
	os.Exit(0)
}

type server struct {
	benchmarks []testing.InternalBenchmark
	benchmem   bool
}

func newServer(m *testing.M) *server {
	v := reflect.ValueOf(m).Elem().FieldByName("benchmarks")
	benchmarks := *(*[]testing.InternalBenchmark)(unsafe.Pointer(v.UnsafeAddr())) // :(((

	s := server{benchmarks: benchmarks}
	return &s
}

func (s *server) serve() {
	cmds := map[string]func([]string){
		"help": s.cmdHelp,
		"quit": s.cmdQuit,
		"exit": s.cmdQuit,
		"list": s.cmdList,
		"run":  s.cmdRun,
		"set":  s.cmdSet,
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			s.cmdHelp(nil)
			continue
		}
		cmd := cmds[fields[0]]
		if cmd == nil {
			s.cmdHelp(nil)
			continue
		}
		cmd(fields[1:])
	}
	if err := scanner.Err(); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
}

func (s *server) cmdHelp([]string) {
	fmt.Fprintln(os.Stderr, "commands: help, list, run, set, quit, exit")
}

func (s *server) cmdQuit([]string) {
	os.Exit(0)
}

func (s *server) cmdList([]string) {
	for _, b := range s.benchmarks {
		fmt.Println(b.Name)
	}
	fmt.Println()
}

func (s *server) cmdSet(args []string) {
	// TODO: What else is worth setting?
	if len(args) < 2 || args[0] != "benchmem" {
		fmt.Fprintln(os.Stderr, "set benchmem <bool>")
		return
	}
	b, err := strconv.ParseBool(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad benchmem value:", err)
		return
	}
	s.benchmem = b
}

func (s *server) cmdRun(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "run <name>[-cpu] <iterations>")
		return
	}

	name := args[0]
	procs := 1
	if i := strings.IndexByte(name, '-'); i != -1 {
		var err error
		procs, err = strconv.Atoi(name[i+1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad cpu value:", err)
			return
		}
		name = name[:i]
	}

	var bench testing.InternalBenchmark
	for _, x := range s.benchmarks {
		if x.Name == name {
			bench = x
			// It is possible to define a benchmark with the same name
			// twice in a single test binary, by defining it once
			// in a regular test package and once in an external test package.
			// If you do that, you probably deserve what happens to you now,
			// namely that we run one of the two, but no guarantees which.
			// If someday we combine multiple packages into a single
			// test binary, then we'll probably need to invoke benchmarks
			// by index rather than by name.
			break
		}
	}
	if bench.Name == "" {
		fmt.Fprintln(os.Stderr, "benchmark not found:", name)
		return
	}

	iters, err := strconv.Atoi(args[1])
	if err != nil || iters <= 0 {
		fmt.Fprintf(os.Stderr, "iterations must be positive, got %v\n", iters)
		return
	}

	benchName := benchmarkName(bench.Name, procs)
	fmt.Print(benchName, "\t")

	runtime.GOMAXPROCS(procs)
	r := runBenchmark(bench, iters)

	if r.Failed {
		fmt.Fprintln(os.Stderr, "--- FAIL:", benchName)
		return
	}
	fmt.Print(r.BenchmarkResult)
	if s.benchmem || r.ShowAllocResult {
		fmt.Print("\t", r.MemString())
	}
	fmt.Println()
	if p := runtime.GOMAXPROCS(-1); p != procs {
		fmt.Fprintf(os.Stderr, "testing: %s left GOMAXPROCS set to %d\n", benchName, p)
	}
}

// benchmarkName returns full name of benchmark including procs suffix.
func benchmarkName(name string, n int) string {
	if n != 1 {
		return fmt.Sprintf("%s-%d", name, n)
	}
	return name
}

type Result struct {
	testing.BenchmarkResult
	Failed          bool
	ShowAllocResult bool
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
	r.Failed = v.FieldByName("failed").Bool()
	r.ShowAllocResult = v.FieldByName("showAllocResult").Bool()
	return r
}
