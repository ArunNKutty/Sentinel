// Command neapp is a stand-in for the NE's Atom daemon.
//
// It proves two things inside a Plasma VM:
//  1. userspace can reach a kernel module over generic netlink (it fetches a
//     device ID from gluon_sim.ko), and
//  2. two VMs can use those kernel-supplied identities to negotiate a shared
//     session key.
//
// Together that is the plumbing the real Atom/kwark/gluon stack needs.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// errENOENT lets gluon.go recognise "family not registered" without importing
// syscall there.
var errENOENT = syscall.ENOENT

const usage = `neapp — NE application stand-in (device ID over gennetlink + peer handshake)

Usage:
  neapp id                     Fetch and print this VM's device ID from gluon_sim.ko
  neapp serve [addr]           Print the device ID and accept handshakes (default :7777)
  neapp handshake <host[:port]>  Connect to a peer and negotiate a session key

The session key printed by both peers must match — that is the proof.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "id":
		err = runID()
	case "serve":
		addr := fmt.Sprintf(":%d", handshakePort)
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		err = runServe(addr)
	case "handshake":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		err = runHandshake(os.Args[2])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runID() error {
	id, err := fetchDeviceID()
	if err != nil {
		return err
	}
	fmt.Printf("device-id: 0x%016x  (from gluon_sim.ko over gennetlink)\n", id)
	return nil
}

func runServe(addr string) error {
	id, err := fetchDeviceID()
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer ln.Close()

	host, _ := os.Hostname()
	fmt.Printf("neapp serving on %s\n", ln.Addr())
	fmt.Printf("  host:      %s\n", host)
	fmt.Printf("  device-id: 0x%016x\n", id)

	// Shut down cleanly on SIGTERM so systemd restarts behave.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				fmt.Println("shutting down")
				return nil
			}
			return err
		}
		// Serial by design: this is a proof, not a server. Each handshake is
		// sub-millisecond, so queueing peers is fine.
		handle(conn, id)
	}
}

func handle(conn net.Conn, localID uint64) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	res, err := exchange(conn, localID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handshake with %s failed: %v\n", conn.RemoteAddr(), err)
		return
	}
	report("accepted handshake from "+conn.RemoteAddr().String(), res)
}

func runHandshake(target string) error {
	// Allow a bare host; default to the well-known port.
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, strconv.Itoa(handshakePort))
	}

	id, err := fetchDeviceID()
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	res, err := exchange(conn, id)
	if err != nil {
		return err
	}
	report("handshake with "+target, res)
	return nil
}

func report(what string, r result) {
	fmt.Printf("\n%s\n", what)
	fmt.Printf("  local  device-id: 0x%016x\n", r.localID)
	fmt.Printf("  peer   device-id: 0x%016x\n", r.peerID)
	fmt.Printf("  session key:      %s\n", hex.EncodeToString(r.sessionKey))
	fmt.Println("  (both peers must print the same session key)")
}
