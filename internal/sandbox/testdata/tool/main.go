// Command tool is a static stand-in for a shell inside test images, which
// have no libc: tool echo|write|cat|env.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "echo":
		fmt.Println(os.Args[2:])
	case "write":
		if err := os.WriteFile(os.Args[2], []byte(os.Args[3]), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "cat":
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Stdout.Write(data)
	case "env":
		fmt.Println(os.Getenv(os.Args[2]))
	}
}
