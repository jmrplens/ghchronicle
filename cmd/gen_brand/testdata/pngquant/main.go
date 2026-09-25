// Command pngquant is the stand-in the tests of gen_brand put on PATH in place
// of the real quantizer. It takes the arguments gen_brand passes,
// `--quality=80-95 --speed=1 --strip --force --output <out.png> -- <in.png>`,
// reads the input and writes it back behind the word "quantized", so the test
// can tell a quantized og image from a copied one and see which file was
// read. Both names are resolved inside the working directory, as bare names
// from gen_brand are.
//
// FAKEPNGQUANT_EXIT makes it end with that status and a message on standard
// error instead: 99 is the real one's "the result would fall below the
// quality asked for".
package main

import (
	"fmt"
	"os"
	"strconv"
)

func main() {
	args := os.Args[1:]
	if status := os.Getenv("FAKEPNGQUANT_EXIT"); status != "" {
		code, err := strconv.Atoi(status)
		if err != nil {
			code = 2
		}
		fmt.Fprintln(os.Stderr, "pngquant: refused as asked")
		os.Exit(code)
	}
	if len(args) != 8 || args[4] != "--output" || args[6] != "--" {
		fmt.Fprintf(os.Stderr, "pngquant: unexpected arguments %q\n", args)
		os.Exit(2)
	}
	dir, err := os.OpenRoot(".")
	var in []byte
	if err == nil {
		in, err = dir.ReadFile(args[7])
	}
	if err == nil {
		err = dir.WriteFile(args[5], append([]byte("quantized "), in...), 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pngquant:", err)
		os.Exit(1)
	}
}
