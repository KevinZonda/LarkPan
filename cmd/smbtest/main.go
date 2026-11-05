// Quick SMB client smoke test: connect, list root + demo/, read a file.
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/hirochachacha/go-smb2"
)

func main() {
	conn, err := net.Dial("tcp", "127.0.0.1:4445")
	must(err)
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{User: "larkpan", Password: "larkpan"},
	}
	s, err := d.Dial(conn)
	must(err)
	defer s.Logoff()
	sh, err := s.Mount(`larkpan`)
	must(err)
	defer sh.Umount()

	ents, err := sh.ReadDir(".")
	must(err)
	for _, e := range ents {
		fmt.Println("dir:", e.Name())
	}
	f, err := sh.Open(`demo\README.md`)
	must(err)
	buf := make([]byte, 128)
	n, err := f.Read(buf)
	must(err)
	fmt.Printf("file: %q\n", buf[:n])
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
}
