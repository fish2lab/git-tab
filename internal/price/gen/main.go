// Command gen rebuilds the built-in price snapshot from models.dev:
//
//	go run ./internal/price/gen [api.json] > internal/price/snapshot.json
//
// With no argument it downloads https://models.dev/api.json.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/fish2lab/git-tab/internal/price"
)

func main() {
	var b []byte
	var err error
	if len(os.Args) > 1 {
		b, err = os.ReadFile(os.Args[1])
	} else {
		var resp *http.Response
		if resp, err = http.Get(price.Source); err == nil {
			defer resp.Body.Close()
			b, err = io.ReadAll(resp.Body)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	c, err := price.FromModelsDev(b)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, _ := json.Marshal(c)
	os.Stdout.Write(out)
}
