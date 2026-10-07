// Command misbehave is a test plugin that tries what a hostile or buggy
// evaluator would.
package main

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"time"

	"github.com/abhishek-rnjn/evals.si/pkg/wasmplugin"
)

var sink [][]byte

func main() {
	wasmplugin.Main(wasmplugin.Plugin{Evaluators: map[string]wasmplugin.Evaluator{
		"test/spin": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			for {
			}
		},
		"test/hog": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			for {
				sink = append(sink, make([]byte, 8<<20))
			}
		},
		"test/crash": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			fmt.Fprintln(os.Stderr, "boom: bad state")
			os.Exit(3)
			return wasmplugin.Result{}
		},
		"test/panic": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			panic("nil map")
		},
		"test/escape": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			_, ferr := os.ReadFile("/etc/passwd")
			_, derr := os.ReadDir(".")
			_, nerr := net.Dial("tcp", "127.0.0.1:80")
			return wasmplugin.Scores(wasmplugin.Score{Label: fmt.Sprintf("file=%v dir=%v net=%v env=%d",
				ferr != nil, derr != nil, nerr != nil, len(os.Environ()))})
		},
		"test/entropy": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			return wasmplugin.Scores(wasmplugin.Score{Label: fmt.Sprintf("%d %d", time.Now().UnixNano(), rand.Int64())})
		},
		"test/shout": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			for i := 0; i < 1<<18; i++ {
				fmt.Print("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			}
			return wasmplugin.Scores(wasmplugin.Pass())
		},
		"test/wrong-count": func(wasmplugin.Record, wasmplugin.Params) wasmplugin.Result {
			return wasmplugin.Scores(wasmplugin.Pass())
		},
	}})
}
